package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/RostislavDugin/managed-valkey/api/internal/store"
	valkeydomain "github.com/RostislavDugin/managed-valkey/api/internal/valkey"
)

type capacityResponse struct {
	User struct {
		Limit capacityResources `json:"limit"`
		Used  capacityResources `json:"used"`
	} `json:"user"`
	Cluster struct {
		Limit capacityResources `json:"limit"`
		Used  capacityResources `json:"used"`
	} `json:"cluster"`
	Instances struct {
		Limit int `json:"limit"`
		Used  int `json:"used"`
	} `json:"instances"`
}

type capacityResources struct {
	VCPU  int `json:"vcpu"`
	RAMGB int `json:"ram_gb"`
}

func Test_GetValkeyCapacity_WithInstancesFromDifferentOwners_ReturnsPersonalAndClusterReserve(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{clusterTopology: &valkeydomain.ClusterTopology{
		NodeCount: 3, NodeCPUMilli: 4000, NodeRAMMiB: 16384,
	}})
	account := app.registerAccount(t, "")
	otherAccount := app.registerAccount(t, "")

	owned := createValkey(t, app, account, map[string]any{
		"name": "owned-ha", "mode": "ha", "vcpu": 1, "ram_gb": 2,
	})
	other := createValkey(t, app, otherAccount, map[string]any{
		"name": "other-single", "vcpu": 1, "ram_gb": 2,
	})
	excluded := createValkey(t, app, otherAccount, map[string]any{
		"name": "deleted-single", "vcpu": 1, "ram_gb": 1,
	})

	now := time.Now().UTC()
	if err := app.database.DB().Model(&store.ValkeyInstance{}).Where("id = ?", owned.ID).UpdateColumns(map[string]any{
		"applied_vcpu": 2, "applied_ram_gb": 4,
	}).Error; err != nil {
		t.Fatalf("подготовить применённый размер HA: %v", err)
	}
	if err := app.database.DB().Model(&store.ValkeyInstance{}).Where("id = ?", other.ID).UpdateColumns(map[string]any{
		"applied_vcpu": 2, "applied_ram_gb": 4, "deletion_requested_at": now,
	}).Error; err != nil {
		t.Fatalf("подготовить неподтверждённое удаление: %v", err)
	}
	if err := app.database.DB().
		Model(&store.ValkeyInstance{}).
		Where("id = ?", excluded.ID).
		UpdateColumns(map[string]any{
			"deletion_requested_at": now, "deleted_at": now,
		}).
		Error; err != nil {
		t.Fatalf("подготовить подтверждённое удаление: %v", err)
	}

	response := app.requestJSON(t, http.MethodGet, "/v1/managed/valkey/capacity", nil, bearer(account.Token))
	assertStatus(t, response, http.StatusOK)
	capacity := decodeResponse[capacityResponse](t, response)

	if capacity.User.Limit != (capacityResources{VCPU: 4, RAMGB: 12}) ||
		capacity.User.Used != (capacityResources{VCPU: 6, RAMGB: 12}) {
		t.Fatalf("неверный личный бюджет: %+v", capacity.User)
	}
	if capacity.Cluster.Limit != (capacityResources{VCPU: 12, RAMGB: 48}) ||
		capacity.Cluster.Used != (capacityResources{VCPU: 8, RAMGB: 16}) {
		t.Fatalf("неверный общий бюджет: %+v", capacity.Cluster)
	}
	if capacity.Instances.Limit != 32 || capacity.Instances.Used != 2 {
		t.Fatalf("неверный предел инстансов: %+v", capacity.Instances)
	}
}

func Test_GetValkeyCapacity_WithoutJWT_ReturnsUnauthorized(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})

	response := app.requestJSON(t, http.MethodGet, "/v1/managed/valkey/capacity", nil, nil)
	assertStatus(t, response, http.StatusUnauthorized)
}
