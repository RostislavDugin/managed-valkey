package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func Test_CheckHealthEndpoint_WithDifferentStatuses_AcceptsOnlySuccessfulResponse(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "ответ 204 означает готовность и принимается", status: http.StatusNoContent},
		{name: "ответ с перенаправлением отклоняется", status: http.StatusFound, wantErr: true},
		{name: "ответ 503 означает неготовность и отклоняется", status: http.StatusServiceUnavailable, wantErr: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.WriteHeader(testCase.status)
			}))
			defer server.Close()

			client := server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			}
			err := check(context.Background(), client, server.URL)
			if (err != nil) != testCase.wantErr {
				t.Errorf("ошибка %v, ожидание ошибки %v", err, testCase.wantErr)
			}
		})
	}
}
