// Command customstore shows how to write your own idempotency.Store.
//
// The library ships only an in-memory store. To share records between several
// instances of a service you need a store backed by Redis, PostgreSQL or
// another shared database. This example implements the same interface on top
// of a plain map and logs every call, which makes the contract visible.
//
// A real implementation has to keep three promises:
//
//   - Acquire is atomic: of many concurrent calls for one key exactly one
//     returns acquired == true. In Redis that is SET key value NX PX ttl;
//     in PostgreSQL INSERT ... ON CONFLICT DO NOTHING.
//   - An in-progress record expires on its own, so a crashed process cannot
//     block a key forever.
//   - Complete stores a copy of the response, and Release removes only an
//     in-progress record.
//
// Run it with: go run ./examples/customstore
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/poseydon/idempotency"
)

// loggingStore is a minimal Store: a map under a mutex. It does not expire
// records; production code must (see the promises above).
type loggingStore struct {
	mu      sync.Mutex
	records map[string]*record
}

type record struct {
	entry idempotency.Entry
}

var _ idempotency.Store = (*loggingStore)(nil)

func newLoggingStore() *loggingStore {
	return &loggingStore{records: make(map[string]*record)}
}

func (s *loggingStore) Acquire(_ context.Context, key, fingerprint string) (idempotency.Entry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if rec, ok := s.records[key]; ok {
		log.Printf("store: Acquire(%q) -> existing, state %s", key, rec.entry.State)
		return rec.entry, false, nil
	}
	entry := idempotency.Entry{State: idempotency.StateInProgress, Fingerprint: fingerprint}
	s.records[key] = &record{entry: entry}
	log.Printf("store: Acquire(%q) -> acquired", key)
	return entry, true, nil
}

func (s *loggingStore) Complete(_ context.Context, key string, resp idempotency.Response) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.records[key]
	if !ok || rec.entry.State != idempotency.StateInProgress {
		return idempotency.ErrNotFound
	}
	rec.entry.State = idempotency.StateCompleted
	rec.entry.Response = &idempotency.Response{ // a copy, never the caller's value
		Status: resp.Status,
		Header: resp.Header.Clone(),
		Body:   bytes.Clone(resp.Body),
	}
	log.Printf("store: Complete(%q) status %d", key, resp.Status)
	return nil
}

func (s *loggingStore) Release(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if rec, ok := s.records[key]; ok && rec.entry.State == idempotency.StateInProgress {
		delete(s.records, key)
		log.Printf("store: Release(%q)", key)
	}
	return nil
}

func newHandler(store idempotency.Store) http.Handler {
	hello := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		log.Print("handler: running")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "created\n")
	})
	return idempotency.New(store)(hello)
}

func send(h http.Handler, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("body"))
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func main() {
	log.SetFlags(0)
	h := newHandler(newLoggingStore())

	for i := 1; i <= 2; i++ {
		w := send(h, "k1")
		fmt.Printf("request %d: %d %q replayed=%q\n\n", i, w.Code, w.Body.String(), w.Header().Get("Idempotent-Replayed"))
	}
}
