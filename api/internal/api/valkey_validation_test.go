package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
	"github.com/RostislavDugin/managed-valkey/api/internal/store"
)

func TestValkeyCreateRejectsMalformedAndSystemFields(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	headers := mergeHeaders(
		bearer(account.Token),
		map[string]string{"Idempotency-Key": uuid.NewString()},
	)
	valid := `{"name":"cache","prefix":"test","mode":"single","vcpu":1,"ram_gb":1,"password":"` + testValkeyPassword + `"}`
	tests := []struct {
		name   string
		body   string
		reason string
	}{
		{name: "пустое тело", body: ""},
		{name: "null", body: "null"},
		{name: "неверный тип", body: strings.Replace(valid, `"vcpu":1`, `"vcpu":"1"`, 1)},
		{name: "дробное целое", body: strings.Replace(valid, `"vcpu":1`, `"vcpu":1.5`, 1)},
		{name: "повторный ключ", body: strings.Replace(valid, `"name":"cache"`, `"name":"cache","name":"other"`, 1)},
		{name: "неизвестное поле", body: strings.TrimSuffix(valid, "}") + `,"owner":"someone"}`},
		{name: "служебное поле", body: strings.TrimSuffix(valid, "}") + `,"status":"running"}`},
		{name: "второй документ", body: valid + `{}`},
		{
			name:   "слишком большое тело",
			body:   strings.TrimSuffix(valid, "}") + `,"padding":"` + strings.Repeat("x", 17000) + `"}`,
			reason: "body_too_large",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			response := app.requestRaw(t, http.MethodPost, "/v1/managed/valkey/instances", testCase.body, headers)
			errorBody := assertError(t, response, http.StatusBadRequest, string(apierr.CodeValidationFailed))
			if testCase.reason != "" && errorBody.Error.Details["reason"] != testCase.reason {
				t.Fatalf("reason=%v, ожидался %s", errorBody.Error.Details["reason"], testCase.reason)
			}
			if containsAny(string(response.Body), testValkeyPassword, strings.Repeat("x", 100)) {
				t.Fatalf("ошибка отражает тело запроса: %s", response.Body)
			}
		})
	}

	assertDatabaseCount(t, app.database.DB().Model(&store.IdempotencyKey{}).Where("user_id = ?", account.ID), 0)
	assertDatabaseCount(t, app.database.DB().Model(&store.ValkeyInstance{}).Where("user_id = ?", account.ID), 0)
}

func TestValkeyFieldValidationUsesExactBoundaries(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	tests := []struct {
		name      string
		overrides map[string]any
		field     string
	}{
		{name: "пустое имя", overrides: map[string]any{"name": ""}, field: "name"},
		{name: "имя длиннее 40", overrides: map[string]any{"name": strings.Repeat("a", 41)}, field: "name"},
		{name: "имя с заглавной", overrides: map[string]any{"name": "Bad"}, field: "name"},
		{name: "имя с дефисом с края", overrides: map[string]any{"name": "-bad"}, field: "name"},
		{name: "короткий prefix", overrides: map[string]any{"prefix": "ab"}, field: "prefix"},
		{name: "длинный prefix", overrides: map[string]any{"prefix": strings.Repeat("a", 21)}, field: "prefix"},
		{name: "неизвестный mode", overrides: map[string]any{"mode": "cluster"}, field: "mode"},
		{name: "пароль 31", overrides: map[string]any{"password": testValkeyPassword[:31]}, field: "password"},
		{name: "пароль 33", overrides: map[string]any{"password": testValkeyPassword + "x"}, field: "password"},
		{
			name:      "пароль с запрещённым символом",
			overrides: map[string]any{"password": testValkeyPassword[:31] + "!"},
			field:     "password",
		},
		{name: "неизвестный размер", overrides: map[string]any{"vcpu": 3, "ram_gb": 3}, field: "size"},
		{name: "размер вне тарифной сетки", overrides: map[string]any{"vcpu": 2, "ram_gb": 4}, field: "size"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			body := map[string]any{
				"name": "valid-cache", "prefix": "test", "mode": "single", "vcpu": 1, "ram_gb": 1,
				"password": testValkeyPassword,
			}
			for key, value := range testCase.overrides {
				body[key] = value
			}
			response := app.requestJSON(t, http.MethodPost, "/v1/managed/valkey/instances", body, mergeHeaders(
				bearer(account.Token),
				map[string]string{"Idempotency-Key": uuid.NewString()},
			))
			errorBody := assertError(t, response, http.StatusBadRequest, string(apierr.CodeValidationFailed))
			fields, ok := errorBody.Error.Details["fields"].(map[string]any)
			if !ok || fields[testCase.field] == nil {
				t.Fatalf("нет ошибки поля %s: %+v", testCase.field, errorBody)
			}
		})
	}

	for _, boundary := range []struct {
		name   string
		prefix string
	}{
		{name: "a", prefix: "abc"},
		{name: strings.Repeat("a", 40), prefix: strings.Repeat("b", 20)},
	} {
		createValkey(t, app, account, map[string]any{"name": boundary.name, "prefix": boundary.prefix})
	}
}

func TestValkeyWhitelistAndMaintenanceValidation(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")
	created := createValkey(t, app, account, map[string]any{"name": "validation-cache"})
	makeValkeyReady(t, app, created.ID)
	base := "/v1/managed/valkey/instances/" + created.ID.String()

	invalidCIDRs := []struct {
		name  string
		cidrs []string
	}{
		{name: "IPv6", cidrs: []string{"2001:db8::/32"}},
		{name: "prefix 33", cidrs: []string{"192.0.2.1/33"}},
		{name: "больше 100 до дедупликации", cidrs: repeatString("192.0.2.1", 101)},
	}
	for _, testCase := range invalidCIDRs {
		t.Run(testCase.name, func(t *testing.T) {
			response := app.requestJSON(t, http.MethodPut, base+"/whitelist", map[string]any{
				"is_whitelist_enabled": false, "whitelist_cidrs": testCase.cidrs,
			}, bearer(account.Token))
			assertError(t, response, http.StatusBadRequest, string(apierr.CodeValidationFailed))
		})
	}

	maintenanceBodies := []string{
		`{"maintenance":{"duration_min":30}}`,
		`{"maintenance":{"dow":0,"hour_utc":0,"duration_min":0}}`,
		`{"maintenance":{"dow":7,"hour_utc":0,"duration_min":30}}`,
		`{"maintenance":{"dow":0,"hour_utc":24,"duration_min":30}}`,
		`{"maintenance":{"dow":0,"hour_utc":0,"duration_min":30,"unknown":1}}`,
	}
	for index, body := range maintenanceBodies {
		t.Run(fmt.Sprintf("maintenance-%d", index), func(t *testing.T) {
			response := app.requestRaw(t, http.MethodPatch, base, body, bearer(account.Token))
			assertError(t, response, http.StatusBadRequest, string(apierr.CodeValidationFailed))
		})
	}

	set := app.requestRaw(
		t,
		http.MethodPatch,
		base,
		`{"maintenance":{"dow":0,"hour_utc":0,"duration_min":1440}}`,
		bearer(account.Token),
	)
	assertStatus(t, set, http.StatusOK)
	cleared := app.requestRaw(t, http.MethodPatch, base, `{"maintenance":null}`, bearer(account.Token))
	assertStatus(t, cleared, http.StatusOK)
	if instance := decodeResponse[map[string]any](t, cleared); instance["maintenance"] != nil {
		t.Fatalf("maintenance не очищен: %+v", instance)
	}
}

func TestValkeyMutationRejectsQueryAndInvalidIDAfterAuthentication(t *testing.T) {
	app := newHTTPTestAPI(t, testAPIConfig{})
	account := app.registerAccount(t, "")

	assertError(
		t,
		app.requestJSON(
			t,
			http.MethodPatch,
			"/v1/managed/valkey/instances/not-a-uuid",
			map[string]any{"name": "x"},
			bearer(account.Token),
		),
		http.StatusBadRequest,
		string(apierr.CodeValidationFailed),
	)
	assertError(
		t,
		app.requestJSON(
			t,
			http.MethodPatch,
			"/v1/managed/valkey/instances/not-a-uuid",
			map[string]any{"name": "x"},
			nil,
		),
		http.StatusUnauthorized,
		string(apierr.CodeUnauthorized),
	)
	assertError(
		t,
		app.requestJSON(t, http.MethodPost, "/v1/managed/valkey/instances?unexpected=1", map[string]any{}, mergeHeaders(
			bearer(account.Token),
			map[string]string{"Idempotency-Key": uuid.NewString()},
		)),
		http.StatusBadRequest,
		string(apierr.CodeValidationFailed),
	)
	assertError(
		t,
		app.requestJSON(t, http.MethodGet, "/v1/managed/valkey/sizes", nil, nil),
		http.StatusUnauthorized,
		string(apierr.CodeUnauthorized),
	)
}

func repeatString(value string, count int) []string {
	values := make([]string, count)
	for index := range values {
		values[index] = value
	}

	return values
}
