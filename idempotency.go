package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"
)

// replayedHeader marks a response replayed from the store. It is the only
// place the name is spelled out.
const replayedHeader = "Idempotent-Replayed"

// maxWaitRounds bounds how many times a waiting request retries Acquire
// after being woken up, so a key that keeps being released and re-acquired
// cannot hold a request forever.
const maxWaitRounds = 3

// New returns middleware that makes mutating requests idempotent.
//
// A request with an idempotency key (by default the Idempotency-Key header) is
// executed once. Repeating it with the same key and the same request replays
// the stored response without calling the handler; the replay carries the
// Idempotent-Replayed: true header. Reusing the key for a different request
// gives 422, and a repeat that arrives while the first request is still
// running gives 409 (or waits, see WithWait).
//
// Requests with other methods, and requests without a key (unless WithRequired
// is set), pass through untouched. New panics if store is nil or an option has
// an invalid value.
func New(store Store, opts ...Option) func(http.Handler) http.Handler {
	if store == nil {
		panic("idempotency: New: store must not be nil")
	}
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	cfg.validate()

	fail := func(w http.ResponseWriter, r *http.Request, err error) {
		if errors.Is(err, ErrInProgress) {
			w.Header().Set("Retry-After", "1")
		}
		cfg.errorHandler(w, r, err)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !cfg.methods[r.Method] {
				next.ServeHTTP(w, r)
				return
			}
			key := r.Header.Get(cfg.header)
			if key == "" {
				if cfg.required {
					fail(w, r, fmt.Errorf("%w: header %s is required", ErrBadKey, cfg.header))
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			if err := validateKey(key); err != nil {
				fail(w, r, err)
				return
			}

			body, err := readBody(w, r, cfg.maxRequestBody)
			if err != nil {
				fail(w, r, err)
				return
			}
			fp := fingerprint(r, body)

			var scope string
			if cfg.scope != nil {
				scope = cfg.scope(r)
			}
			sk := storeKey(scope, key)

			entry, acquired, err := acquire(r.Context(), store, cfg.wait, sk, fp)
			if err != nil {
				if cfg.failOpen {
					next.ServeHTTP(w, r)
					return
				}
				fail(w, r, err)
				return
			}
			if !acquired {
				// The fingerprint is checked before the state: a request with
				// a different body must not get 409 just because the first
				// one is still running.
				switch {
				case entry.Fingerprint != fp:
					fail(w, r, ErrKeyMismatch)
				case entry.State == StateInProgress:
					fail(w, r, ErrInProgress)
				default:
					replay(w, entry.Response)
				}
				return
			}

			// Complete and Release must survive the client disconnecting.
			ctx := context.WithoutCancel(r.Context())

			// If the handler panics, finished stays false and the deferred
			// Release frees the key. No recover is needed: the panic keeps
			// going and net/http logs it and closes the connection.
			finished := false
			defer func() {
				if !finished {
					_ = store.Release(ctx, sk)
				}
			}()

			rec := newRecorder(w, cfg.maxResponseBody)
			next.ServeHTTP(rec, r)
			finished = true

			// The response is already sent, so a failing store call cannot be
			// reported to the client. A record that could not be completed is
			// released, and as a last resort expires after the lock timeout.
			resp, ok := rec.result()
			if ok && cfg.cacheable(resp.Status) {
				if err := store.Complete(ctx, sk, resp); err != nil {
					_ = store.Release(ctx, sk)
				}
				return
			}
			_ = store.Release(ctx, sk)
		})
	}
}

// acquire calls store.Acquire. If wait is positive, the store implements
// Waiter and the same request is still in progress, it waits for it to finish
// (at most wait in total) and tries again. It returns the last Acquire result,
// so a timeout ends up as an in-progress entry, which becomes 409.
func acquire(ctx context.Context, store Store, wait time.Duration, key, fp string) (Entry, bool, error) {
	waiter, canWait := store.(Waiter)
	waitCtx := ctx
	if wait > 0 && canWait {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}

	for round := 0; ; round++ {
		entry, acquired, err := store.Acquire(ctx, key, fp)
		if err != nil || acquired {
			return entry, acquired, err
		}
		stillRunning := entry.Fingerprint == fp && entry.State == StateInProgress
		if !stillRunning || wait <= 0 || !canWait || round >= maxWaitRounds {
			return entry, acquired, nil
		}
		if waiter.Wait(waitCtx, key) != nil {
			// Timed out or the client went away: report the key as busy.
			return entry, acquired, nil
		}
	}
}

// writeError is the default error handler. It maps a package error to a JSON
// response. Unknown errors are reported with a fixed text so internals do not
// leak.
func writeError(w http.ResponseWriter, err error) {
	status, msg := http.StatusInternalServerError, "internal error"
	switch {
	case errors.Is(err, ErrBadKey):
		status, msg = http.StatusBadRequest, err.Error()
	case errors.Is(err, ErrBadBody):
		status, msg = http.StatusBadRequest, ErrBadBody.Error()
	case errors.Is(err, ErrBodyTooLarge):
		status, msg = http.StatusRequestEntityTooLarge, err.Error()
	case errors.Is(err, ErrKeyMismatch):
		status, msg = http.StatusUnprocessableEntity, err.Error()
	case errors.Is(err, ErrInProgress):
		status, msg = http.StatusConflict, err.Error()
	case errors.Is(err, ErrStoreFull):
		status, msg = http.StatusServiceUnavailable, ErrStoreFull.Error()
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{msg})
}

// defaultErrorHandler is the ErrorHandler used unless WithErrorHandler is set.
func defaultErrorHandler(w http.ResponseWriter, _ *http.Request, err error) {
	writeError(w, err)
}

// replay writes a stored response. Headers must be set before WriteHeader,
// otherwise they never reach the client.
func replay(w http.ResponseWriter, resp *Response) {
	h := w.Header()
	for k, v := range resp.Header {
		h[k] = slices.Clone(v)
	}
	h.Set(replayedHeader, "true")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}
