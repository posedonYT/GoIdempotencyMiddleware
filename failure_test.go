package idempotency

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPanicReleasesKey(t *testing.T) {
	var calls atomic.Int32
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		_, _ = w.Write([]byte("ok"))
	})
	h := New(NewMemoryStore())(next)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic was swallowed, it must propagate to net/http")
			}
		}()
		do(h, http.MethodPost, "/", "k1", "x")
	}()

	// The key was released, so the retry runs the handler, not 409.
	w := do(h, http.MethodPost, "/", "k1", "x")
	if w.Code != http.StatusOK || w.Body.String() != "ok" || calls.Load() != 2 {
		t.Errorf("retry: status = %d, body = %q, calls = %d; want 200, ok, 2", w.Code, w.Body.String(), calls.Load())
	}
}

func TestServerErrorIsNotCached(t *testing.T) {
	var calls atomic.Int32
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	h := New(NewMemoryStore())(next)

	first := do(h, http.MethodPost, "/", "k1", "x")
	if first.Code != http.StatusServiceUnavailable {
		t.Fatalf("first: status = %d, want the handler's 503", first.Code)
	}
	second := do(h, http.MethodPost, "/", "k1", "x")
	if second.Code != http.StatusOK || calls.Load() != 2 {
		t.Errorf("retry: status = %d, calls = %d; want 200, 2", second.Code, calls.Load())
	}
	if _, ok := second.Header()[replayedHeader]; ok {
		t.Error("the retry was replayed")
	}
}

func TestClientErrorIsCached(t *testing.T) {
	next := &statusHandler{status: http.StatusBadRequest}
	h := New(NewMemoryStore())(next)

	do(h, http.MethodPost, "/", "k1", "x")
	w := do(h, http.MethodPost, "/", "k1", "x")

	if w.Code != http.StatusBadRequest || next.calls.Load() != 1 {
		t.Errorf("status = %d, calls = %d; want a replayed 400 and 1 call", w.Code, next.calls.Load())
	}
}

type statusHandler struct {
	calls  atomic.Int32
	status int
}

func (s *statusHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.calls.Add(1)
	w.WriteHeader(s.status)
}

func TestWithCacheable(t *testing.T) {
	// Cache only 201: a 200 is released and runs again.
	only201 := func(status int) bool { return status == http.StatusCreated }

	t.Run("accepted status is stored", func(t *testing.T) {
		next := &statusHandler{status: http.StatusCreated}
		h := New(NewMemoryStore(), WithCacheable(only201))(next)

		do(h, http.MethodPost, "/", "k1", "x")
		do(h, http.MethodPost, "/", "k1", "x")
		if next.calls.Load() != 1 {
			t.Errorf("calls = %d, want 1", next.calls.Load())
		}
	})

	t.Run("rejected status is not", func(t *testing.T) {
		next := &statusHandler{status: http.StatusOK}
		h := New(NewMemoryStore(), WithCacheable(only201))(next)

		do(h, http.MethodPost, "/", "k1", "x")
		do(h, http.MethodPost, "/", "k1", "x")
		if next.calls.Load() != 2 {
			t.Errorf("calls = %d, want 2", next.calls.Load())
		}
	})
}

func TestClientDisconnectStillCompletes(t *testing.T) {
	next := &statusHandler{status: http.StatusCreated}
	h := New(NewMemoryStore())(next)

	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("x")).WithContext(ctx)
	r.Header.Set("Idempotency-Key", "k1")
	// The client goes away before the handler finishes.
	cancel()
	h.ServeHTTP(httptest.NewRecorder(), r)

	w := do(h, http.MethodPost, "/", "k1", "x")
	if w.Header().Get(replayedHeader) != "true" || next.calls.Load() != 1 {
		t.Errorf("replayed = %q, calls = %d; want a replay after the disconnect and 1 call",
			w.Header().Get(replayedHeader), next.calls.Load())
	}
}

// brokenStore lets a test fail individual Store calls.
type brokenStore struct {
	Store
	acquireErr  error
	completeErr error
	released    atomic.Int32
}

func (b *brokenStore) Acquire(ctx context.Context, key, fp string) (Entry, bool, error) {
	if b.acquireErr != nil {
		return Entry{}, false, b.acquireErr
	}
	return b.Store.Acquire(ctx, key, fp)
}

func (b *brokenStore) Complete(ctx context.Context, key string, resp Response) error {
	if b.completeErr != nil {
		return b.completeErr
	}
	return b.Store.Complete(ctx, key, resp)
}

func (b *brokenStore) Release(ctx context.Context, key string) error {
	b.released.Add(1)
	return b.Store.Release(ctx, key)
}

func TestStoreFailurePolicy(t *testing.T) {
	storeErr := errors.New("redis is down")

	t.Run("fail closed is the default", func(t *testing.T) {
		next := &createdHandler{}
		h := New(&brokenStore{Store: NewMemoryStore(), acquireErr: storeErr})(next)

		w := do(h, http.MethodPost, "/", "k1", "x")
		if w.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", w.Code)
		}
		if strings.Contains(w.Body.String(), "redis") {
			t.Errorf("body %q leaks the store error", w.Body.String())
		}
		if next.calls.Load() != 0 {
			t.Errorf("handler called %d times, want 0", next.calls.Load())
		}
	})

	t.Run("fail open runs the handler", func(t *testing.T) {
		next := &createdHandler{}
		h := New(&brokenStore{Store: NewMemoryStore(), acquireErr: storeErr}, WithFailOpen(true))(next)

		w := do(h, http.MethodPost, "/", "k1", `{"a":1}`)
		if w.Code != http.StatusCreated || next.calls.Load() != 1 {
			t.Errorf("status = %d, calls = %d; want 201, 1", w.Code, next.calls.Load())
		}
		next.mu.Lock()
		defer next.mu.Unlock()
		if next.gotBody != `{"a":1}` {
			t.Errorf("handler saw body %q, the body must be restored", next.gotBody)
		}
	})

	t.Run("failed Complete releases the key", func(t *testing.T) {
		st := &brokenStore{Store: NewMemoryStore(), completeErr: storeErr}
		next := &createdHandler{}
		h := New(st)(next)

		if w := do(h, http.MethodPost, "/", "k1", "x"); w.Code != http.StatusCreated {
			t.Fatalf("status = %d, the client must still get the response", w.Code)
		}
		do(h, http.MethodPost, "/", "k1", "x")
		if next.calls.Load() != 2 {
			t.Errorf("handler called %d times, want 2 (key released, not stuck in progress)", next.calls.Load())
		}
	})
}

func TestStoreFullIs503(t *testing.T) {
	store := NewMemoryStore(WithMaxEntries(1))
	h := New(store)(&createdHandler{})

	do(h, http.MethodPost, "/", "k1", "x")
	w := do(h, http.MethodPost, "/", "k2", "x")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestWithErrorHandler(t *testing.T) {
	var got []error
	h := New(NewMemoryStore(), WithRequired(true),
		WithErrorHandler(func(w http.ResponseWriter, _ *http.Request, err error) {
			got = append(got, err)
			w.WriteHeader(http.StatusTeapot)
		}))(&createdHandler{})

	if w := do(h, http.MethodPost, "/", "", "x"); w.Code != http.StatusTeapot {
		t.Errorf("missing key: status = %d, want the custom 418", w.Code)
	}
	do(h, http.MethodPost, "/", "k1", "x")
	if w := do(h, http.MethodPost, "/", "k1", "other"); w.Code != http.StatusTeapot {
		t.Errorf("mismatch: status = %d, want the custom 418", w.Code)
	}

	if len(got) != 2 || !errors.Is(got[0], ErrBadKey) || !errors.Is(got[1], ErrKeyMismatch) {
		t.Errorf("handler got %v, want [ErrBadKey, ErrKeyMismatch]", got)
	}
}

func TestInProgressSetsRetryAfterEvenWithCustomHandler(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	})
	h := New(NewMemoryStore(), WithErrorHandler(func(w http.ResponseWriter, _ *http.Request, _ error) {
		w.WriteHeader(http.StatusConflict)
	}))(next)

	done := make(chan struct{})
	go func() { do(h, http.MethodPost, "/", "k1", "x"); close(done) }()
	<-entered

	w := do(h, http.MethodPost, "/", "k1", "x")
	close(release)
	<-done

	if w.Header().Get("Retry-After") != "1" {
		t.Errorf("Retry-After = %q, want 1", w.Header().Get("Retry-After"))
	}
}
