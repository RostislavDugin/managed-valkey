package api_test

import (
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/audit"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeydomain "github.com/RostislavDugin/managed-valkey/api/internal/valkey"
)

func Test_CreateValkeyInstance_WithLegacyInsertWithoutUserEmail_UsesOwnerEmailAndRejectsUnknownOwner(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	created := createValkey(t, app, account, map[string]any{"name": "source", "prefix": "source"})
	legacy := loadValkey(t, app, created.ID)
	legacy.ID = uuid.Nil
	legacy.Name = "legacy"
	legacy.Slug = "legacy-" + uuid.NewString()[:8]
	legacy.Host = legacy.Slug + ".valkey.localhost"
	legacy.UserEmail = ""

	if err := app.database.DB().Omit("UserEmail").Create(&legacy).Error; err != nil {
		t.Fatalf("создать инстанс старым INSERT: %v", err)
	}
	stored := loadValkey(t, app, legacy.ID)
	if stored.UserEmail != account.Email {
		t.Fatalf("триггер сохранил email %q, ожидался %q", stored.UserEmail, account.Email)
	}

	unknownOwner := legacy
	unknownOwner.ID = uuid.Nil
	unknownOwner.UserID = uuid.New()
	unknownOwner.Name = "unknown-owner"
	unknownOwner.Slug = "unknown-" + uuid.NewString()[:8]
	unknownOwner.Host = unknownOwner.Slug + ".valkey.localhost"
	if err := app.database.DB().Omit("UserEmail").Create(&unknownOwner).Error; err == nil {
		t.Fatal("старый INSERT создал инстанс для неизвестного пользователя")
	}
	var count int64
	if err := app.database.DB().
		Model(&store.ValkeyInstance{}).
		Where("id = ?", unknownOwner.ID).
		Count(&count).
		Error; err != nil {
		t.Fatalf("проверить отсутствие инстанса: %v", err)
	}
	if count != 0 {
		t.Fatalf("для неизвестного пользователя создано строк: %d", count)
	}
}

type sequenceSlugGenerator struct {
	mu     sync.Mutex
	values []string
	index  int
}

func (g *sequenceSlugGenerator) Suffix() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	index := min(g.index, len(g.values)-1)
	value := g.values[index]
	g.index++

	return value, nil
}

func Test_CreateValkey_WithSlugCollisions_RetriesAtomicallyWithoutPartialWrites(t *testing.T) {
	generator := &sequenceSlugGenerator{values: []string{"aaaaaa", "aaaaaa", "bbbbbb"}}
	app := newHTTPTestAPI(t, testAPIConfig{slugGenerator: generator})
	account := app.registerAccount(t, "")
	setUserQuota(t, app, account.ID, 32, 128)

	first := createValkey(t, app, account, map[string]any{"name": "first-slug", "prefix": "shop"})
	second := createValkey(t, app, account, map[string]any{"name": "second-slug", "prefix": "shop"})
	if first.Slug != "shop-aaaaaa" || second.Slug != "shop-bbbbbb" {
		t.Fatalf("коллизия slug обработана неверно: %s, %s", first.Slug, second.Slug)
	}
	assertDatabaseCount(t, app.database.DB().Model(&store.ValkeyInstance{}).Where("user_id = ?", account.ID), 2)
	assertDatabaseCount(t, app.database.DB().Model(&store.BillingPeriod{}).Where("user_id = ?", account.ID), 2)
	assertDatabaseCount(
		t,
		app.database.DB().Model(&store.AuditLog{}).
			Where("user_id = ? AND action = ?", account.ID, audit.ActionInstanceCreate),
		2,
	)

	beforeKeys := countRows(t, app, &store.IdempotencyKey{}, "user_id = ?", account.ID)
	failure := app.requestJSON(t, http.MethodPost, "/v1/managed/valkey/instances", map[string]any{
		"name": "exhausted-slug", "prefix": "shop", "mode": "single", "vcpu": 1, "ram_gb": 1,
		"password": testValkeyPassword,
	}, mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": uuid.NewString()}))
	assertError(t, failure, http.StatusInternalServerError, string(apierr.CodeInternal))
	assertDatabaseCount(t, app.database.DB().Model(&store.ValkeyInstance{}).Where("user_id = ?", account.ID), 2)
	if countRows(t, app, &store.IdempotencyKey{}, "user_id = ?", account.ID) != beforeKeys {
		t.Fatal("исчерпание slug сохранило ключ")
	}
}

func Test_CreateValkey_WithRepeatedName_ConflictsOnlyForActiveOwnerRows(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	owner := app.registerAccount(t, "")
	other := app.registerAccount(t, "")
	created := createValkey(t, app, owner, map[string]any{"name": "shared-name", "prefix": "owner"})

	conflict := createValkeyResponse(t, app, owner, "shared-name", "owner")
	conflictBody := assertError(t, conflict, http.StatusConflict, string(apierr.CodeConflict))
	if conflictBody.Error.Details["reason"] != "name_taken" || conflictBody.Error.Details["field"] != "name" {
		t.Fatalf("неверный конфликт имени: %+v", conflictBody)
	}
	assertStatus(t, createValkeyResponse(t, app, other, "shared-name", "other"), http.StatusAccepted)

	base := "/v1/managed/valkey/instances/" + created.ID.String()
	assertStatus(t, app.requestJSON(t, http.MethodDelete, base, nil, bearer(owner.Token)), http.StatusAccepted)
	assertError(
		t,
		createValkeyResponse(t, app, owner, "shared-name", "owner"),
		http.StatusConflict,
		string(apierr.CodeConflict),
	)
	if err := app.database.DB().Model(&store.ValkeyInstance{}).Where("id = ?", created.ID).
		UpdateColumn("deleted_at", created.UpdatedAt).Error; err != nil {
		t.Fatalf("подтвердить удаление: %v", err)
	}
	reused := createValkeyResponse(t, app, owner, "shared-name", "owner")
	assertStatus(t, reused, http.StatusAccepted)
	if next := decodeResponse[valkeydomain.Instance](t, reused); next.Slug == created.Slug {
		t.Fatalf("slug удалённой базы переиспользован: %s", next.Slug)
	}
}

func Test_ResizeValkey_AfterRateChange_PreservesOldBillingPeriodPrice(t *testing.T) {
	initialCatalog := mustCatalog(t, 125, 50)
	initial := newHTTPTestAPI(t, testAPIConfig{catalog: &initialCatalog})
	account := initial.registerAccount(t, "")
	setUserQuota(t, initial, account.ID, 16, 64)
	created := createValkey(t, initial, account, map[string]any{
		"name": "priced-cache", "vcpu": 1, "ram_gb": 2,
	})
	makeValkeyReady(t, initial, created.ID)

	newCatalog := mustCatalog(t, 200, 80)
	updatedRates := newHTTPTestAPI(t, testAPIConfig{catalog: &newCatalog})
	response := updatedRates.requestJSON(
		t,
		http.MethodPost,
		"/v1/managed/valkey/instances/"+created.ID.String()+"/resize",
		map[string]any{"vcpu": 2, "ram_gb": 8},
		mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": uuid.NewString()}),
	)
	assertStatus(t, response, http.StatusAccepted)

	var periods []store.BillingPeriod
	if err := initial.database.DB().
		Where("resource_id = ?", created.ID).
		Order("started_at").
		Find(&periods).
		Error; err != nil {
		t.Fatalf("прочитать периоды: %v", err)
	}
	if len(periods) != 2 || periods[0].PriceCoinsPerHour != 225 || periods[1].PriceCoinsPerHour != 1040 {
		t.Fatalf("ставки периодов переписаны неверно: %+v", periods)
	}
}

func createValkeyResponse(t *testing.T, app *testAPI, account testAccount, name, prefix string) testResponse {
	t.Helper()

	return app.requestJSON(t, http.MethodPost, "/v1/managed/valkey/instances", map[string]any{
		"name": name, "prefix": prefix, "mode": "single", "vcpu": 1, "ram_gb": 1,
		"password": testValkeyPassword,
	}, mergeHeaders(bearer(account.Token), map[string]string{"Idempotency-Key": uuid.NewString()}))
}

func mustCatalog(t *testing.T, vcpuRate, ramRate int64) valkeydomain.Catalog {
	t.Helper()

	catalog, err := valkeydomain.NewCatalog(valkeydomain.CatalogConfig{
		MaxVCPU: 16, MaxRAMGB: 128, VCPUCoinsPerHour: vcpuRate, RAMGBCoinsPerHour: ramRate,
		Domain: "valkey.localhost", Port: 41379,
	})
	if err != nil {
		t.Fatalf("создать каталог: %v", err)
	}

	return catalog
}
