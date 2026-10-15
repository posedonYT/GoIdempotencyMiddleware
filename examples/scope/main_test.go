package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/poseydon/idempotency"
)

func post(h http.Handler, user, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader("tea"))
	if user != "" {
		r.Header.Set(userHeader, user)
	}
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestKeysAreIsolatedPerUser(t *testing.T) {
	store := idempotency.NewMemoryStore()
	defer func() { _ = store.Close() }()
	h := newHandler(store)

	alice := post(h, "alice", "k1")
	bob := post(h, "bob", "k1")
	if alice.Body.String() == bob.Body.String() {
		t.Errorf("alice and bob got the same response %q, scopes are not isolated", alice.Body.String())
	}

	again := post(h, "alice", "k1")
	if again.Body.String() != alice.Body.String() || again.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("alice's repeat = %q (replayed %q), want a replay of %q",
			again.Body.String(), again.Header().Get("Idempotent-Replayed"), alice.Body.String())
	}
}

func TestAnonymousRequestsAreRejected(t *testing.T) {
	store := idempotency.NewMemoryStore()
	defer func() { _ = store.Close() }()

	if w := post(newHandler(store), "", "k1"); w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestDifferentKeysAreDifferentOrders(t *testing.T) {
	store := idempotency.NewMemoryStore()
	defer func() { _ = store.Close() }()
	h := newHandler(store)

	if a, b := post(h, "alice", "k1"), post(h, "alice", "k2"); a.Body.String() == b.Body.String() {
		t.Errorf("two keys gave one response %q", a.Body.String())
	}
}
