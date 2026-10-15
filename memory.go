package idempotency

import (
	"bytes"
	"context"
	"sync"
	"time"
)

const (
	defaultTTL             = 24 * time.Hour
	defaultLockTimeout     = 30 * time.Second
	defaultCleanupInterval = time.Minute
)

// MemoryStore is an in-memory Store safe for concurrent use.
// Records live only in the memory of this process: they are lost on restart
// and are not visible to other processes or instances, so it is not suitable
// for deployments with more than one replica.
//
// Expired records are removed lazily when their key is touched and by a
// background goroutine, so call Close when the store is no longer needed.
// Without WithMaxEntries the store grows with the number of live keys.
type MemoryStore struct {
	mu              sync.Mutex
	items           map[string]*item
	ttl             time.Duration
	lockTimeout     time.Duration
	cleanupInterval time.Duration
	maxEntries      int
	now             func() time.Time

	stop      chan struct{}
	stopped   chan struct{} // closed when the janitor has exited
	closeOnce sync.Once
}

type item struct {
	state       State
	fingerprint string
	response    *Response
	expiresAt   time.Time
	// done is closed when an in-progress record is completed, released or
	// expires. Closing works as a broadcast for every waiter.
	done chan struct{}
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

// WithCleanupInterval sets how often the background goroutine removes expired
// records. A value of zero or less disables it; expired records are then only
// removed when their key is used again. The default is one minute.
func WithCleanupInterval(d time.Duration) MemoryOption {
	return func(s *MemoryStore) {
		s.cleanupInterval = d
	}
}

// WithMaxEntries limits the number of records. When the limit is reached and
// no expired record can be dropped, Acquire returns ErrStoreFull. Zero (the
// default) means no limit.
func WithMaxEntries(n int) MemoryOption {
	return func(s *MemoryStore) {
		s.maxEntries = n
	}
}

// withClock replaces the clock. It is used by tests.
func withClock(now func() time.Time) MemoryOption {
	return func(s *MemoryStore) {
		s.now = now
	}
}

var (
	_ Store  = (*MemoryStore)(nil)
	_ Waiter = (*MemoryStore)(nil)
)

// NewMemoryStore returns an empty MemoryStore with the default TTL and lock
// timeout, overridden by opts. It panics on an invalid option value (a
// non-positive TTL or lock timeout, or a negative entry limit).
func NewMemoryStore(opts ...MemoryOption) *MemoryStore {
	s := &MemoryStore{
		items:           make(map[string]*item),
		ttl:             defaultTTL,
		lockTimeout:     defaultLockTimeout,
		cleanupInterval: defaultCleanupInterval,
		now:             time.Now,
		stop:            make(chan struct{}),
		stopped:         make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	switch {
	case s.ttl <= 0:
		panic("idempotency: WithTTL: ttl must be positive")
	case s.lockTimeout <= 0:
		panic("idempotency: WithLockTimeout: timeout must be positive")
	case s.maxEntries < 0:
		panic("idempotency: WithMaxEntries: limit must not be negative")
	case s.now == nil:
		panic("idempotency: clock must not be nil")
	}

	if s.cleanupInterval > 0 {
		go s.janitor(s.cleanupInterval)
	} else {
		close(s.stopped)
	}
	return s
}

// janitor periodically removes expired records until Close is called.
func (s *MemoryStore) janitor(interval time.Duration) {
	defer close(s.stopped)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.sweep()
		case <-s.stop:
			return
		}
	}
}

// sweep removes every expired record.
func (s *MemoryStore) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(s.now())
}

// sweepLocked removes expired records. The caller must hold s.mu.
func (s *MemoryStore) sweepLocked(now time.Time) {
	for key, it := range s.items {
		if !now.Before(it.expiresAt) {
			s.removeLocked(key, it)
		}
	}
}

// removeLocked deletes a record and wakes its waiters if it was still in
// progress. The caller must hold s.mu.
func (s *MemoryStore) removeLocked(key string, it *item) {
	if it.state == StateInProgress {
		close(it.done)
	}
	delete(s.items, key)
}

// Close stops the background cleanup goroutine and waits for it to exit.
// It is safe to call more than once. The store keeps working afterwards, but
// expired records are only removed lazily.
func (s *MemoryStore) Close() error {
	s.closeOnce.Do(func() { close(s.stop) })
	<-s.stopped
	return nil
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
		s.removeLocked(key, it)
		ok = false
	}
	if !ok {
		if s.maxEntries > 0 && len(s.items) >= s.maxEntries {
			s.sweepLocked(now)
			if len(s.items) >= s.maxEntries {
				return Entry{}, false, ErrStoreFull
			}
		}
		s.items[key] = &item{
			state:       StateInProgress,
			fingerprint: fingerprint,
			expiresAt:   now.Add(s.lockTimeout),
			done:        make(chan struct{}),
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
	close(it.done)
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
		s.removeLocked(key, it)
	}
	return nil
}

// Wait blocks until the in-progress record for key is completed, released or
// expires, or until ctx is done. It returns nil at once if there is no
// in-progress record. It implements Waiter.
func (s *MemoryStore) Wait(ctx context.Context, key string) error {
	s.mu.Lock()
	var done chan struct{}
	if it, ok := s.items[key]; ok && it.state == StateInProgress {
		done = it.done
	}
	s.mu.Unlock()

	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
