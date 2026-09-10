package apierr_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/RostislavDugin/managed-valkey/api/internal/apierr"
)

func Test_WriteApiError_WithKnownCodes_ReturnsMappedHttpResponse(t *testing.T) {
	cases := []struct {
		code apierr.Code
		want int
	}{
		{code: apierr.CodeValidationFailed, want: http.StatusBadRequest},
		{code: apierr.CodeUnauthorized, want: http.StatusUnauthorized},
		{code: apierr.CodeNotFound, want: http.StatusNotFound},
		{code: apierr.CodeConflict, want: http.StatusConflict},
		{code: apierr.CodeOperationInProgress, want: http.StatusConflict},
		{code: apierr.CodeInstanceNotReady, want: http.StatusConflict},
		{code: apierr.CodeIdempotencyMismatch, want: http.StatusUnprocessableEntity},
		{code: apierr.CodeQuotaExceeded, want: http.StatusUnprocessableEntity},
		{code: apierr.CodeNotEnoughResources, want: http.StatusUnprocessableEntity},
		{code: apierr.CodeRateLimited, want: http.StatusTooManyRequests},
		{code: apierr.CodeUnavailable, want: http.StatusServiceUnavailable},
	}

	for _, testCase := range cases {
		t.Run(string(testCase.code), func(t *testing.T) {
			response := write(t, apierr.New(testCase.code, "сообщение", map[string]any{"field": "email"}))

			if response.Code != testCase.want {
				t.Errorf("код %d, ожидался %d", response.Code, testCase.want)
			}
			if body := response.Body.String(); body != `{"error":{"code":"`+string(
				testCase.code,
			)+`","details":{"field":"email"},"message":"сообщение"}}` {
				t.Errorf("неожиданное тело %s", body)
			}
		})
	}
}

func Test_WriteApiError_WithUnknownError_ReturnsSafeInternalResponse(t *testing.T) {
	response := write(t, errors.New("пароль и токен не должны попасть в ответ"))

	if response.Code != http.StatusInternalServerError {
		t.Errorf("код %d, ожидался %d", response.Code, http.StatusInternalServerError)
	}
	if body := response.Body.String(); body != `{"error":{"code":"INTERNAL","details":{},"message":"Внутренняя ошибка сервера"}}` {
		t.Errorf("неожиданное тело %s", body)
	}
}

func write(t *testing.T, err error) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	apierr.Write(c, err)

	return response
}
