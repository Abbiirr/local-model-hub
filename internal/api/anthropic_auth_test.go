package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicAuthAcceptsBearerOrAPIKey(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := authMiddleware("s3cret", next)

	tests := []struct {
		name   string
		header string
		value  string
		extra  map[string]string
		want   int
	}{
		{name: "bearer", header: "Authorization", value: "Bearer s3cret", want: http.StatusNoContent},
		{name: "api key", header: "x-api-key", value: "s3cret", want: http.StatusNoContent},
		{name: "matching both", header: "Authorization", value: "Bearer s3cret",
			extra: map[string]string{"x-api-key": "s3cret"}, want: http.StatusNoContent},
		{name: "mismatched both", header: "Authorization", value: "Bearer s3cret",
			extra: map[string]string{"x-api-key": "wrong"}, want: http.StatusUnauthorized},
		{name: "bad key", header: "x-api-key", value: "wrong", want: http.StatusUnauthorized},
		{name: "missing", want: http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(`{}`))
			if tc.header != "" {
				req.Header.Set(tc.header, tc.value)
			}
			for key, value := range tc.extra {
				req.Header.Set(key, value)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusUnauthorized &&
				(!strings.Contains(rec.Body.String(), `"type":"error"`) ||
					!strings.Contains(rec.Body.String(), `"type":"authentication_error"`)) {
				t.Fatalf("not an Anthropic authentication error: %s", rec.Body.String())
			}
		})
	}
}

func TestAnthropicAPIKeyDoesNotAuthenticateOpenAIPath(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", "s3cret")
	rec := httptest.NewRecorder()
	authMiddleware("s3cret", next).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("x-api-key unexpectedly authenticated OpenAI route: %d", rec.Code)
	}
}
