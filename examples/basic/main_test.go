package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/poseydon/idempotency"
)

func post(h http.Handler, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/payments", strings.NewReader(body))
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRepeatedPaymentIsReplayed(t *testing.T) {
	store := idempotency.NewMemoryStore()
	defer func() { _ = store.Close() }()
	h := newHandler(store)

	first := post(h, "order-1", `{"amount": 500}`)
	second := post(h, "order-1", `{"amount": 500}`)

	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("statuses = %d, %d; want 201, 201", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Errorf("bodies differ: %q vs %q", first.Body.String(), second.Body.String())
	}
	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Error("the second response is not marked as replayed")
	}

	if w := post(h, "order-1", `{"amount": 999}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("key reuse: status = %d, want 422", w.Code)
	}
}
