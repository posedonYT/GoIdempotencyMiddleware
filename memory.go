package idempotency

import (
	"bytes"
	"context"
	"sync"
	"time"
)

const (
	defaultTTL         = 24 * time.Hour
	defaultLockTimeout = 30 * time.Second
)

// MemoryStore is an in-memory Store safe for concurrent use.
// Records live only in the memory of this process: they are lost on restart
// and are not visible to other processes or instances, so it is not suitable
// for deployments with more than one replica.
type MemoryStore struct {
	mu          sync.Mutex
	items       map[string]*item
	ttl         time.Duration
	lockTimeout time.Duration
	now         func() time.Time
}

type item struct {
	state       State
	fingerprint string
	response    *Response
	expiresAt   time.Time
}

// MemoryOption configures a MemoryStore in NewMemoryStore.
type MemoryOption func(*MemoryStore)

// WithTTL sets how long a completed record is kept for replay.
// The default is 24 hours.
func WithTTL(d time.Duration) MemoryOption {
	return func(s *MemoryStore) {
		s.ttl = d
	}
}

// WithLockTimeout sets how long an in-progress record holds its key.
// If the handler hangs or the process dies, the key is freed after this time.
// The default is 30 seconds.
func WithLockTimeout(d time.Duration) MemoryOption {
	return func(s *MemoryStore) {
		s.lockTimeout = d
	}
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore returns an empty MemoryStore with the default TTL and lock
// timeout, overridden by opts.
func NewMemoryStore(opts ...MemoryOption) *MemoryStore {
	s := &MemoryStore{
		items:       make(map[string]*item),
		ttl:         defaultTTL,
		lockTimeout: defaultLockTimeout,
		now:         time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Acquire inserts an in-progress record for key, or returns the existing one.
//
// The lookup and the insert happen under a single lock. This is the whole
// point of the method: if the lock were released between checking the key and
// inserting it, two concurrent requests could both see no record, both insert
// one, and both run the handler, so one key would be acquired twice.
//
// An expired record is treated as absent. The returned Entry is a copy, so
// the caller never touches the store's internal state.
func (s *MemoryStore) Acquire(_ context.Context, key, fingerprint string) (Entry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	it, ok := s.items[key]
	if ok && !now.Before(it.expiresAt) {
		delete(s.items, key)
		ok = false
	}
	if !ok {
		s.items[key] = &item{
			state:       StateInProgress,
			fingerprint: fingerprint,
			expiresAt:   now.Add(s.lockTimeout),
		}
		return Entry{State: StateInProgress, Fingerprint: fingerprint}, true, nil
	}
	return Entry{State: it.state, Fingerprint: it.fingerprint, Response: it.response}, false, nil
}

// Complete stores a copy of resp, moves the record to StateCompleted and
// starts its TTL. It returns ErrNotFound if there is no in-progress record
// for key.
func (s *MemoryStore) Complete(_ context.Context, key string, resp Response) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[key]
	if !ok || it.state != StateInProgress {
		return ErrNotFound
	}
	it.state = StateCompleted
	it.response = &Response{
		Status: resp.Status,
		Header: resp.Header.Clone(),
		Body:   bytes.Clone(resp.Body),
	}
	it.expiresAt = s.now().Add(s.ttl)
	return nil
}

// Release deletes an in-progress record so the key can be acquired again.
// Completed records are kept. A missing key is not an error.
func (s *MemoryStore) Release(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if it, ok := s.items[key]; ok && it.state == StateInProgress {
		delete(s.items, key)
	}
	return nil
}
