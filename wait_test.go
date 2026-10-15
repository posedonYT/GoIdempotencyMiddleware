package idempotency

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// spyStore reports on waiting so tests know a request is blocked in Wait.
type spyStore struct {
	*MemoryStore
	waiting chan struct{}
}

func newSpyStore() *spyStore {
	return &spyStore{MemoryStore: NewMemoryStore(), waiting: make(chan struct{}, 100)}
}

func (s *spyStore) Wait(ctx context.Context, key string) error {
	s.waiting <- struct{}{}
	return s.MemoryStore.Wait(ctx, key)
}

// blockingHandler blocks the first call until release is closed.
type blockingHandler struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
	// firstStatus is the status of the first call, later calls answer 200 "ok".
	firstStatus int
}

func newBlockingHandler(firstStatus int) *blockingHandler {
	return &blockingHandler{entered: make(chan struct{}), release: make(chan struct{}), firstStatus: firstStatus}
}

func (b *blockingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if b.calls.Add(1) == 1 {
		close(b.entered)
		<-b.release
		w.WriteHeader(b.firstStatus)
		_, _ = w.Write([]byte("first"))
		return
	}
	_, _ = w.Write([]byte("ok"))
}

func TestWaitReplaysTheFirstResponse(t *testing.T) {
	store := newSpyStore()
	next := newBlockingHandler(http.StatusCreated)
	h := New(store, WithWait(5*time.Second))(next)

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- do(h, http.MethodPost, "/", "k1", "x") }()
	<-next.entered

	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- do(h, http.MethodPost, "/", "k1", "x") }()
	<-store.waiting // the second request is now blocked in Wait
	close(next.release)

	w1, w2 := <-first, <-second
	if w2.Code != http.StatusCreated || w2.Body.String() != "first" {
		t.Errorf("second: status = %d, body = %q; want the first response", w2.Code, w2.Body.String())
	}
	if w2.Header().Get(replayedHeader) != "true" {
		t.Error("second response is not marked as replayed")
	}
	if w1.Header().Get(replayedHeader) != "" {
		t.Error("first response is marked as replayed")
	}
	if next.calls.Load() != 1 {
		t.Errorf("handler called %d times, want 1", next.calls.Load())
	}
}

func TestWaitTimeoutGives409(t *testing.T) {
	next := newBlockingHandler(http.StatusOK)
	h := New(NewMemoryStore(), WithWait(20*time.Millisecond))(next)

	first := make(chan struct{})
	go func() { do(h, http.MethodPost, "/", "k1", "x"); close(first) }()
	<-next.entered

	w := do(h, http.MethodPost, "/", "k1", "x")
	close(next.release)
	<-first

	if w.Code != http.StatusConflict || w.Header().Get("Retry-After") == "" {
		t.Errorf("status = %d, Retry-After = %q; want 409 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
}

func TestWaiterTakesOverAfterRelease(t *testing.T) {
	// The first request fails with 500, which releases the key: the waiting
	// request must become the new executor instead of failing.
	store := newSpyStore()
	next := newBlockingHandler(http.StatusInternalServerError)
	h := New(store, WithWait(5*time.Second))(next)

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- do(h, http.MethodPost, "/", "k1", "x") }()
	<-next.entered

	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- do(h, http.MethodPost, "/", "k1", "x") }()
	<-store.waiting
	close(next.release)

	if w := <-first; w.Code != http.StatusInternalServerError {
		t.Errorf("first: status = %d, want 500", w.Code)
	}
	w := <-second
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Errorf("second: status = %d, body = %q; want its own 200 ok", w.Code, w.Body.String())
	}
	if next.calls.Load() != 2 {
		t.Errorf("handler called %d times, want 2", next.calls.Load())
	}
}

func TestWaitStopsWhenClientLeaves(t *testing.T) {
	store := newSpyStore()
	next := newBlockingHandler(http.StatusOK)
	h := New(store, WithWait(time.Minute))(next)

	first := make(chan struct{})
	go func() { do(h, http.MethodPost, "/", "k1", "x"); close(first) }()
	<-next.entered

	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("x")).WithContext(ctx)
	r.Header.Set("Idempotency-Key", "k1")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(w, r); close(done) }()
	<-store.waiting
	cancel()
	<-done // returns although the first request is still running

	close(next.release)
	<-first
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

// plainStore hides everything but the Store methods, so it is not a Waiter.
type plainStore struct{ Store }

func TestWaitNeedsAWaiter(t *testing.T) {
	next := newBlockingHandler(http.StatusOK)
	h := New(plainStore{NewMemoryStore()}, WithWait(time.Minute))(next)

	first := make(chan struct{})
	go func() { do(h, http.MethodPost, "/", "k1", "x"); close(first) }()
	<-next.entered

	// Must answer at once, not after a minute.
	w := do(h, http.MethodPost, "/", "k1", "x")
	close(next.release)
	<-first
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

func TestWaitDoesNotApplyToAnotherRequest(t *testing.T) {
	// A different body is a key reuse error and must not wait.
	next := newBlockingHandler(http.StatusOK)
	h := New(NewMemoryStore(), WithWait(time.Minute))(next)

	first := make(chan struct{})
	go func() { do(h, http.MethodPost, "/", "k1", "x"); close(first) }()
	<-next.entered

	w := do(h, http.MethodPost, "/", "k1", "other")
	close(next.release)
	<-first
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", w.Code)
	}
}

func TestConcurrentSameKey(t *testing.T) {
	const n = 50

	run := func(t *testing.T, opts ...Option) (calls int32, codes map[int]int) {
		t.Helper()
		var count atomic.Int32
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			count.Add(1)
			time.Sleep(50 * time.Millisecond) // make the requests overlap
			_, _ = w.Write([]byte("ok"))
		})
		h := New(NewMemoryStore(), opts...)(next)

		start := make(chan struct{})
		var (
			mu sync.Mutex
			wg sync.WaitGroup
		)
		codes = map[int]int{}
		for range n {
			wg.Go(func() {
				<-start
				w := do(h, http.MethodPost, "/", "k1", "x")
				mu.Lock()
				codes[w.Code]++
				mu.Unlock()
				if w.Code == http.StatusOK && w.Body.String() != "ok" {
					t.Errorf("body = %q, want ok", w.Body.String())
				}
			})
		}
		close(start)
		wg.Wait()
		return count.Load(), codes
	}

	t.Run("with wait every request gets 200", func(t *testing.T) {
		calls, codes := run(t, WithWait(10*time.Second))
		if calls != 1 {
			t.Errorf("handler called %d times, want 1", calls)
		}
		if codes[http.StatusOK] != n {
			t.Errorf("codes = %v, want %d x 200", codes, n)
		}
	})

	t.Run("without wait the rest get 200 or 409", func(t *testing.T) {
		calls, codes := run(t)
		if calls != 1 {
			t.Errorf("handler called %d times, want 1", calls)
		}
		if codes[http.StatusOK]+codes[http.StatusConflict] != n || codes[http.StatusOK] < 1 {
			t.Errorf("codes = %v, want only 200 and 409 and at least one 200", codes)
		}
	})
}
