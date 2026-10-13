package idempotency

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// callCounter is a handler that records how many times it was called.
type callCounter struct{ calls atomic.Int32 }

func (c *callCounter) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	c.calls.Add(1)
	w.WriteHeader(http.StatusTeapot)
}

func serve(h http.Handler, method, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/", nil)
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestMiddlewarePassesThroughOtherMethods(t *testing.T) {
	next := &callCounter{}
	h := New(NewMemoryStore())(next)

	// GET is not in the default methods, so the key is not even looked at.
	for _, key := range []string{"", "valid-key", "bad key"} {
		w := serve(h, http.MethodGet, key)
		if w.Code != http.StatusTeapot {
			t.Errorf("GET key=%q: status = %d, want %d", key, w.Code, http.StatusTeapot)
		}
	}
	if next.calls.Load() != 3 {
		t.Errorf("next called %d times, want 3", next.calls.Load())
	}
}

func TestMiddlewareRespectsWithMethods(t *testing.T) {
	next := &callCounter{}
	h := New(NewMemoryStore(), WithMethods(http.MethodPut))(next)

	// POST is no longer handled by the middleware, so it passes through.
	if w := serve(h, http.MethodPost, "bad key"); w.Code != http.StatusTeapot {
		t.Errorf("POST status = %d, want passthrough %d", w.Code, http.StatusTeapot)
	}
	// PUT is handled, so the bad key is rejected.
	if w := serve(h, http.MethodPut, "bad key"); w.Code != http.StatusBadRequest {
		t.Errorf("PUT status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestMiddlewareMissingKey(t *testing.T) {
	t.Run("optional passes through", func(t *testing.T) {
		next := &callCounter{}
		h := New(NewMemoryStore())(next)

		w := serve(h, http.MethodPost, "")
		if w.Code != http.StatusTeapot || next.calls.Load() != 1 {
			t.Errorf("status = %d, calls = %d; want %d, 1", w.Code, next.calls.Load(), http.StatusTeapot)
		}
	})

	t.Run("required is rejected", func(t *testing.T) {
		next := &callCounter{}
		h := New(NewMemoryStore(), WithRequired(true))(next)

		w := serve(h, http.MethodPost, "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		if !strings.Contains(w.Body.String(), "Idempotency-Key") {
			t.Errorf("body = %q, want it to name the header", w.Body.String())
		}
		if next.calls.Load() != 0 {
			t.Errorf("next called %d times, want 0", next.calls.Load())
		}
	})

	t.Run("custom header name in message", func(t *testing.T) {
		h := New(NewMemoryStore(), WithRequired(true), WithHeader("X-Key"))(&callCounter{})

		w := serve(h, http.MethodPost, "")
		if !strings.Contains(w.Body.String(), "X-Key") {
			t.Errorf("body = %q, want it to name X-Key", w.Body.String())
		}
	})
}

func TestMiddlewareInvalidKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"space", "bad key"},
		{"too long", strings.Repeat("a", maxKeyLen+1)},
		{"non-ascii", "ключ"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			next := &callCounter{}
			h := New(NewMemoryStore())(next)

			r := httptest.NewRequest(http.MethodPost, "/", nil)
			// Set directly: Header.Set would also work, but this keeps the
			// raw value intact whatever it contains.
			r.Header["Idempotency-Key"] = []string{tc.key}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
			}
			if !strings.Contains(w.Body.String(), ErrBadKey.Error()) {
				t.Errorf("body = %q, want the ErrBadKey message", w.Body.String())
			}
			if next.calls.Load() != 0 {
				t.Errorf("next called %d times, want 0", next.calls.Load())
			}
		})
	}
}

func TestMiddlewareCustomHeader(t *testing.T) {
	h := New(NewMemoryStore(), WithHeader("X-Request-Id"))(&callCounter{})

	// The default header is ignored when another one is configured.
	if w := serve(h, http.MethodPost, "bad key"); w.Code != http.StatusTeapot {
		t.Errorf("default header: status = %d, want passthrough %d", w.Code, http.StatusTeapot)
	}

	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-Request-Id", "bad key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("custom header: status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestWriteError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantRetry  string
		wantBody   string
	}{
		{"bad key", ErrBadKey, http.StatusBadRequest, "", ErrBadKey.Error()},
		{"wrapped bad key", fmt.Errorf("%w: empty", ErrBadKey), http.StatusBadRequest, "", "empty"},
		{"key mismatch", ErrKeyMismatch, http.StatusUnprocessableEntity, "", ErrKeyMismatch.Error()},
		{"in progress", ErrInProgress, http.StatusConflict, "1", ErrInProgress.Error()},
		{"wrapped in progress", fmt.Errorf("acquire: %w", ErrInProgress), http.StatusConflict, "1", "acquire"},
		{"unknown", errors.New("db password is hunter2"), http.StatusInternalServerError, "", "internal error"},
		{"not found is internal", ErrNotFound, http.StatusInternalServerError, "", "internal error"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeError(w, tc.err)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if got := w.Header().Get("Retry-After"); got != tc.wantRetry {
				t.Errorf("Retry-After = %q, want %q", got, tc.wantRetry)
			}
			if !strings.Contains(w.Body.String(), tc.wantBody) {
				t.Errorf("body = %q, want it to contain %q", w.Body.String(), tc.wantBody)
			}
		})
	}
}

func TestWriteErrorDoesNotLeakInternals(t *testing.T) {
	w := httptest.NewRecorder()
	writeError(w, errors.New("secret detail"))

	if strings.Contains(w.Body.String(), "secret") {
		t.Errorf("body = %q leaks the error text", w.Body.String())
	}
}

func TestMiddlewareValidKeyReachesHandler(t *testing.T) {
	next := &callCounter{}
	h := New(NewMemoryStore())(next)

	w := serve(h, http.MethodPost, "valid-key")
	if w.Code != http.StatusTeapot || next.calls.Load() != 1 {
		t.Errorf("status = %d, calls = %d; want %d, 1", w.Code, next.calls.Load(), http.StatusTeapot)
	}
}

func TestReplayedHeaderName(t *testing.T) {
	if replayedHeader != "Idempotent-Replayed" {
		t.Errorf("replayedHeader = %q, want %q", replayedHeader, "Idempotent-Replayed")
	}
	if replayedHeader != http.CanonicalHeaderKey(replayedHeader) {
		t.Errorf("replayedHeader %q is not canonical", replayedHeader)
	}
}

// do sends a request through h. extra holds header name/value pairs.
func do(h http.Handler, method, target, key, body string, extra ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		r.Header.Set(extra[i], extra[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// createdHandler answers 201 with a custom header and a JSON body, and counts
// its calls and remembers the request body it saw.
type createdHandler struct {
	calls   atomic.Int32
	mu      sync.Mutex
	gotBody string
}

func (c *createdHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.calls.Add(1)
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.gotBody = string(b)
	c.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Custom", "yes")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"id":1}`))
}

func TestFirstRequestExecutes(t *testing.T) {
	next := &createdHandler{}
	h := New(NewMemoryStore())(next)

	w := do(h, http.MethodPost, "/orders", "k1", `{"a":1}`)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}
	if w.Body.String() != `{"id":1}` {
		t.Errorf("body = %q", w.Body.String())
	}
	if w.Header().Get("X-Custom") != "yes" {
		t.Errorf("X-Custom = %q, want yes", w.Header().Get("X-Custom"))
	}
	if _, ok := w.Header()[replayedHeader]; ok {
		t.Errorf("%s present on the first response", replayedHeader)
	}
	if next.calls.Load() != 1 {
		t.Errorf("handler called %d times, want 1", next.calls.Load())
	}
}

func TestRepeatIsReplayed(t *testing.T) {
	next := &createdHandler{}
	h := New(NewMemoryStore())(next)

	first := do(h, http.MethodPost, "/orders", "k1", `{"a":1}`)
	second := do(h, http.MethodPost, "/orders", "k1", `{"a":1}`)

	if next.calls.Load() != 1 {
		t.Fatalf("handler called %d times, want 1", next.calls.Load())
	}
	if second.Code != first.Code {
		t.Errorf("status = %d, want %d", second.Code, first.Code)
	}
	if second.Body.String() != first.Body.String() {
		t.Errorf("body = %q, want %q", second.Body.String(), first.Body.String())
	}
	for _, name := range []string{"X-Custom", "Content-Type"} {
		if got, want := second.Header().Get(name), first.Header().Get(name); got != want || got == "" {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := second.Header().Get(replayedHeader); got != "true" {
		t.Errorf("%s = %q, want true", replayedHeader, got)
	}
}

func TestHandlerSeesRequestBody(t *testing.T) {
	next := &createdHandler{}
	h := New(NewMemoryStore())(next)

	do(h, http.MethodPost, "/orders", "k1", `{"a":1}`)

	next.mu.Lock()
	defer next.mu.Unlock()
	if next.gotBody != `{"a":1}` {
		t.Errorf("handler saw body %q, want %q", next.gotBody, `{"a":1}`)
	}
}

func TestScopeSeparatesKeys(t *testing.T) {
	next := &createdHandler{}
	h := New(NewMemoryStore(), WithScope(func(r *http.Request) string {
		return r.Header.Get("X-User")
	}))(next)

	do(h, http.MethodPost, "/orders", "k1", "x", "X-User", "alice")
	do(h, http.MethodPost, "/orders", "k1", "x", "X-User", "bob")
	if next.calls.Load() != 2 {
		t.Fatalf("handler called %d times, want 2 (same key, different scopes)", next.calls.Load())
	}

	w := do(h, http.MethodPost, "/orders", "k1", "x", "X-User", "alice")
	if next.calls.Load() != 2 {
		t.Errorf("handler called %d times after alice's repeat, want 2", next.calls.Load())
	}
	if w.Header().Get(replayedHeader) != "true" {
		t.Error("alice's repeat was not replayed")
	}
}

func TestSameKeyDifferentRequestIsMismatch(t *testing.T) {
	tests := []struct {
		name                 string
		method, target, body string
	}{
		{"other body", http.MethodPost, "/orders", `{"a":2}`},
		{"other path", http.MethodPost, "/payments", `{"a":1}`},
		{"other query", http.MethodPost, "/orders?x=1", `{"a":1}`},
		{"other method", http.MethodPatch, "/orders", `{"a":1}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			next := &createdHandler{}
			h := New(NewMemoryStore())(next)

			do(h, http.MethodPost, "/orders", "k1", `{"a":1}`)
			w := do(h, tc.method, tc.target, "k1", tc.body)

			if w.Code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want %d", w.Code, http.StatusUnprocessableEntity)
			}
			if next.calls.Load() != 1 {
				t.Errorf("handler called %d times, want 1", next.calls.Load())
			}
		})
	}
}

func TestInProgressThenReplay(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		close(entered)
		<-release
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("done"))
	})
	h := New(NewMemoryStore())(next)

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- do(h, http.MethodPost, "/", "k1", "x") }()
	<-entered

	w := do(h, http.MethodPost, "/", "k1", "x")
	if w.Code != http.StatusConflict {
		t.Errorf("second request: status = %d, want %d", w.Code, http.StatusConflict)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("second request: no Retry-After")
	}

	// A different body gets 422, not 409, while the first request still runs.
	if w := do(h, http.MethodPost, "/", "k1", "other"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("different body: status = %d, want %d", w.Code, http.StatusUnprocessableEntity)
	}

	close(release)
	<-firstDone

	w = do(h, http.MethodPost, "/", "k1", "x")
	if w.Code != http.StatusCreated || w.Body.String() != "done" {
		t.Errorf("third request: status = %d, body = %q; want replay", w.Code, w.Body.String())
	}
	if w.Header().Get(replayedHeader) != "true" {
		t.Error("third request was not marked as replayed")
	}
	if calls.Load() != 1 {
		t.Errorf("handler called %d times, want 1", calls.Load())
	}
}

func TestUnreadableBody(t *testing.T) {
	next := &createdHandler{}
	h := New(NewMemoryStore(), WithMaxRequestBody(4))(next)

	w := do(h, http.MethodPost, "/", "k1", "too long body")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if next.calls.Load() != 0 {
		t.Errorf("handler called %d times, want 0", next.calls.Load())
	}
}

// failingStore fails every Acquire.
type failingStore struct{ Store }

func (failingStore) Acquire(context.Context, string, string) (Entry, bool, error) {
	return Entry{}, false, errors.New("store down")
}

func TestAcquireErrorIs500(t *testing.T) {
	next := &createdHandler{}
	h := New(failingStore{})(next)

	w := do(h, http.MethodPost, "/", "k1", "x")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
	if next.calls.Load() != 0 {
		t.Errorf("handler called %d times, want 0", next.calls.Load())
	}
}

func TestOversizedResponseIsNotStored(t *testing.T) {
	var calls atomic.Int32
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte("0123456789"))
	})
	h := New(NewMemoryStore(), WithMaxResponseBody(4))(next)

	w := do(h, http.MethodPost, "/", "k1", "x")
	if w.Body.String() != "0123456789" {
		t.Errorf("body = %q, client must still get the full response", w.Body.String())
	}
	// Temporary behaviour until pass 3: the record stays in progress.
	if w := do(h, http.MethodPost, "/", "k1", "x"); w.Code != http.StatusConflict {
		t.Errorf("repeat: status = %d, want %d", w.Code, http.StatusConflict)
	}
}
