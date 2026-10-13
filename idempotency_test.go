package idempotency

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
