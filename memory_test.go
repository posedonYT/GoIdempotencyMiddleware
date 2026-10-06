package idempotency

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for tests without time.Sleep.
type fakeClock struct {
	mu  sync.Mutex
	cur time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{cur: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur = c.cur.Add(d)
}

func newTestStore(opts ...MemoryOption) (*MemoryStore, *fakeClock) {
	clk := newFakeClock()
	s := NewMemoryStore(opts...)
	s.now = clk.Now
	return s, clk
}

func testResponse() Response {
	return Response{
		Status: http.StatusCreated,
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   []byte(`{"id":1}`),
	}
}

func TestNewMemoryStoreDefaults(t *testing.T) {
	s := NewMemoryStore()
	if s.ttl != defaultTTL {
		t.Errorf("ttl = %v, want %v", s.ttl, defaultTTL)
	}
	if s.lockTimeout != defaultLockTimeout {
		t.Errorf("lockTimeout = %v, want %v", s.lockTimeout, defaultLockTimeout)
	}
	if s.now == nil {
		t.Error("now is nil")
	}
	if s.items == nil {
		t.Error("items is nil")
	}
}

func TestNewMemoryStoreOptions(t *testing.T) {
	s := NewMemoryStore(WithTTL(time.Minute), WithLockTimeout(time.Second))
	if s.ttl != time.Minute {
		t.Errorf("ttl = %v, want %v", s.ttl, time.Minute)
	}
	if s.lockTimeout != time.Second {
		t.Errorf("lockTimeout = %v, want %v", s.lockTimeout, time.Second)
	}
}

func TestAcquireNewKey(t *testing.T) {
	s, _ := newTestStore()

	e, acquired, err := s.Acquire(context.Background(), "k", "fp")
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if !acquired {
		t.Error("acquired = false, want true")
	}
	if e.State != StateInProgress {
		t.Errorf("State = %v, want %v", e.State, StateInProgress)
	}
	if e.Fingerprint != "fp" {
		t.Errorf("Fingerprint = %q, want %q", e.Fingerprint, "fp")
	}
	if e.Response != nil {
		t.Errorf("Response = %v, want nil", e.Response)
	}
}

func TestAcquireExistingInProgress(t *testing.T) {
	s, _ := newTestStore()
	ctx := context.Background()

	if _, _, err := s.Acquire(ctx, "k", "fp1"); err != nil {
		t.Fatal(err)
	}

	// The second caller sees the first record, even with another fingerprint.
	e, acquired, err := s.Acquire(ctx, "k", "fp2")
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if acquired {
		t.Error("acquired = true, want false")
	}
	if e.State != StateInProgress {
		t.Errorf("State = %v, want %v", e.State, StateInProgress)
	}
	if e.Fingerprint != "fp1" {
		t.Errorf("Fingerprint = %q, want the original %q", e.Fingerprint, "fp1")
	}
}

func TestAcquireKeysAreIndependent(t *testing.T) {
	s, _ := newTestStore()
	ctx := context.Background()

	for _, key := range []string{"a", "b"} {
		_, acquired, err := s.Acquire(ctx, key, "fp")
		if err != nil || !acquired {
			t.Errorf("Acquire(%q) = acquired %v, err %v; want true, nil", key, acquired, err)
		}
	}
}

func TestAcquireCompleted(t *testing.T) {
	s, _ := newTestStore()
	ctx := context.Background()

	if _, _, err := s.Acquire(ctx, "k", "fp"); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, "k", testResponse()); err != nil {
		t.Fatal(err)
	}

	e, acquired, err := s.Acquire(ctx, "k", "fp")
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if acquired {
		t.Error("acquired = true, want false")
	}
	if e.State != StateCompleted {
		t.Errorf("State = %v, want %v", e.State, StateCompleted)
	}
	if e.Response == nil {
		t.Fatal("Response = nil, want stored response")
	}
	want := testResponse()
	if e.Response.Status != want.Status {
		t.Errorf("Status = %d, want %d", e.Response.Status, want.Status)
	}
	if got := e.Response.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if string(e.Response.Body) != string(want.Body) {
		t.Errorf("Body = %q, want %q", e.Response.Body, want.Body)
	}
}

func TestLockTimeoutExpiry(t *testing.T) {
	const lockTimeout = 10 * time.Second
	s, clk := newTestStore(WithLockTimeout(lockTimeout))
	ctx := context.Background()

	if _, _, err := s.Acquire(ctx, "k", "fp1"); err != nil {
		t.Fatal(err)
	}

	// Just before the timeout the key is still held.
	clk.Advance(lockTimeout - time.Nanosecond)
	if _, acquired, _ := s.Acquire(ctx, "k", "fp2"); acquired {
		t.Fatal("acquired before lock timeout, want false")
	}

	// At exactly lockTimeout the record is expired and can be taken again.
	clk.Advance(time.Nanosecond)
	e, acquired, err := s.Acquire(ctx, "k", "fp2")
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if !acquired {
		t.Fatal("acquired = false after lock timeout, want true")
	}
	if e.Fingerprint != "fp2" {
		t.Errorf("Fingerprint = %q, want %q", e.Fingerprint, "fp2")
	}
}

func TestTTLExpiry(t *testing.T) {
	const ttl = time.Hour
	s, clk := newTestStore(WithTTL(ttl))
	ctx := context.Background()

	if _, _, err := s.Acquire(ctx, "k", "fp"); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, "k", testResponse()); err != nil {
		t.Fatal(err)
	}

	clk.Advance(ttl - time.Nanosecond)
	if e, acquired, _ := s.Acquire(ctx, "k", "fp"); acquired || e.State != StateCompleted {
		t.Fatalf("before ttl: acquired %v, state %v; want false, %v", acquired, e.State, StateCompleted)
	}

	clk.Advance(time.Nanosecond)
	e, acquired, err := s.Acquire(ctx, "k", "fp")
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if !acquired || e.State != StateInProgress {
		t.Errorf("after ttl: acquired %v, state %v; want true, %v", acquired, e.State, StateInProgress)
	}
}

func TestCompleteRestartsTTL(t *testing.T) {
	// Complete must count the TTL from completion, not from Acquire,
	// otherwise a slow handler would shorten the replay window.
	const (
		lockTimeout = time.Minute
		ttl         = time.Hour
	)
	s, clk := newTestStore(WithLockTimeout(lockTimeout), WithTTL(ttl))
	ctx := context.Background()

	if _, _, err := s.Acquire(ctx, "k", "fp"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(lockTimeout / 2)
	if err := s.Complete(ctx, "k", testResponse()); err != nil {
		t.Fatal(err)
	}

	// Past the original lock timeout the completed record must survive.
	clk.Advance(lockTimeout)
	if e, acquired, _ := s.Acquire(ctx, "k", "fp"); acquired || e.State != StateCompleted {
		t.Errorf("acquired %v, state %v; want false, %v", acquired, e.State, StateCompleted)
	}
}

func TestCompleteErrors(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name  string
		setup func(s *MemoryStore)
	}{
		{"missing key", func(s *MemoryStore) {}},
		{"already completed", func(s *MemoryStore) {
			s.Acquire(ctx, "k", "fp")
			s.Complete(ctx, "k", testResponse())
		}},
		{"released", func(s *MemoryStore) {
			s.Acquire(ctx, "k", "fp")
			s.Release(ctx, "k")
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestStore()
			tc.setup(s)
			if err := s.Complete(ctx, "k", testResponse()); !errors.Is(err, ErrNotFound) {
				t.Errorf("Complete() error = %v, want %v", err, ErrNotFound)
			}
		})
	}
}

func TestCompleteCopiesResponse(t *testing.T) {
	s, _ := newTestStore()
	ctx := context.Background()

	if _, _, err := s.Acquire(ctx, "k", "fp"); err != nil {
		t.Fatal(err)
	}
	resp := testResponse()
	if err := s.Complete(ctx, "k", resp); err != nil {
		t.Fatal(err)
	}

	// Mutating the caller's response after Complete must not affect the store.
	resp.Body[0] = 'X'
	resp.Header.Set("Content-Type", "text/plain")

	e, _, err := s.Acquire(ctx, "k", "fp")
	if err != nil {
		t.Fatal(err)
	}
	if string(e.Response.Body) != `{"id":1}` {
		t.Errorf("Body = %q, store kept a reference to the caller's slice", e.Response.Body)
	}
	if got := e.Response.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, store kept a reference to the caller's header", got)
	}
}

func TestReleaseInProgress(t *testing.T) {
	s, _ := newTestStore()
	ctx := context.Background()

	if _, _, err := s.Acquire(ctx, "k", "fp"); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "k"); err != nil {
		t.Fatalf("Release() error = %v", err)
	}

	_, acquired, err := s.Acquire(ctx, "k", "fp")
	if err != nil || !acquired {
		t.Errorf("Acquire() after Release = acquired %v, err %v; want true, nil", acquired, err)
	}
}

func TestReleaseKeepsCompleted(t *testing.T) {
	s, _ := newTestStore()
	ctx := context.Background()

	if _, _, err := s.Acquire(ctx, "k", "fp"); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, "k", testResponse()); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "k"); err != nil {
		t.Fatalf("Release() error = %v", err)
	}

	e, acquired, err := s.Acquire(ctx, "k", "fp")
	if err != nil {
		t.Fatal(err)
	}
	if acquired || e.State != StateCompleted {
		t.Errorf("acquired %v, state %v; want false, %v", acquired, e.State, StateCompleted)
	}
}

func TestReleaseMissingKey(t *testing.T) {
	s, _ := newTestStore()
	if err := s.Release(context.Background(), "nope"); err != nil {
		t.Errorf("Release() error = %v, want nil", err)
	}
}

// TestAcquireConcurrent checks the check-then-insert race: of many goroutines
// acquiring one key, exactly one may win. Run with -race.
func TestAcquireConcurrent(t *testing.T) {
	const goroutines = 100
	s := NewMemoryStore()
	ctx := context.Background()

	var (
		wins  atomic.Int32
		start = make(chan struct{})
		wg    sync.WaitGroup
	)
	for range goroutines {
		wg.Go(func() {
			<-start
			_, acquired, err := s.Acquire(ctx, "k", "fp")
			if err != nil {
				t.Errorf("Acquire() error = %v", err)
				return
			}
			if acquired {
				wins.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Errorf("%d goroutines acquired the key, want exactly 1", got)
	}
}

func TestMemoryStoreConcurrentMixed(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			key := string(rune('a' + i%5))
			if _, acquired, _ := s.Acquire(ctx, key, "fp"); acquired {
				if i%2 == 0 {
					s.Complete(ctx, key, testResponse())
				} else {
					s.Release(ctx, key)
				}
			}
		})
	}
	wg.Wait()
}
