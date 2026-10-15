package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/poseydon/idempotency"
)

func do(h http.Handler, method, target, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader("tea"))
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRouter(t *testing.T) {
	store := idempotency.NewMemoryStore()
	defer func() { _ = store.Close() }()
	h := newHandler(store)

	t.Run("health is not protected", func(t *testing.T) {
		if w := do(h, http.MethodGet, "/health", ""); w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	t.Run("key is required in the API", func(t *testing.T) {
		w := do(h, http.MethodPost, "/api/orders", "")
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "bad_idempotency_key") {
			t.Errorf("status = %d, body = %q; want 400 with the custom code", w.Code, w.Body.String())
		}
	})

	t.Run("repeat is replayed", func(t *testing.T) {
		first := do(h, http.MethodPost, "/api/orders", "k1")
		second := do(h, http.MethodPost, "/api/orders", "k1")
		if first.Body.String() != second.Body.String() || second.Header().Get("Idempotent-Replayed") != "true" {
			t.Errorf("first %q, second %q (replayed %q)", first.Body.String(), second.Body.String(),
				second.Header().Get("Idempotent-Replayed"))
		}
	})

	t.Run("PUT is protected too", func(t *testing.T) {
		do(h, http.MethodPut, "/api/orders/7", "k2")
		if w := do(h, http.MethodPut, "/api/orders/8", "k2"); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422 for the same key on another path", w.Code)
		}
	})
}
