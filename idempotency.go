package idempotency

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
)

// replayedHeader marks a response replayed from the store. It is the only
// place the name is spelled out.
const replayedHeader = "Idempotent-Replayed"

func New(store Store, opts ...Option) func(http.Handler) http.Handler {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
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
					writeError(w, fmt.Errorf("%w: header %s is required", ErrBadKey, cfg.header))
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			if err := validateKey(key); err != nil {
				writeError(w, err)
				return
			}

			body, err := readBody(w, r, cfg.maxRequestBody)
			if err != nil {
				// TODO(pass 3): 413 for a body over the limit.
				http.Error(w, "cannot read request body", http.StatusBadRequest)
				return
			}
			fp := fingerprint(r, body)

			var scope string
			if cfg.scope != nil {
				scope = cfg.scope(r)
			}
			sk := storeKey(scope, key)

			entry, acquired, err := store.Acquire(r.Context(), sk, fp)
			if err != nil {
				writeError(w, err)
				return
			}
			if !acquired {
				// The fingerprint is checked before the state: a request with
				// a different body must not get 409 just because the first
				// one is still running.
				switch {
				case entry.Fingerprint != fp:
					writeError(w, ErrKeyMismatch)
				case entry.State == StateInProgress:
					writeError(w, ErrInProgress)
				default:
					replay(w, entry.Response)
				}
				return
			}

			// Complete must survive the client disconnecting mid-request.
			ctx := context.WithoutCancel(r.Context())
			rec := newRecorder(w, cfg.maxResponseBody)
			next.ServeHTTP(rec, r)

			// If the response did not fit the limit (ok == false) the record
			// stays in progress until lockTimeout.
			// TODO(pass 3): release the key in that case.
			if resp, ok := rec.result(); ok {
				// The response is already sent, so there is nobody to report
				// a failed Complete to.
				_ = store.Complete(ctx, sk, resp)
			}
		})
	}
}

// writeError maps a package error to an HTTP response.
// Unknown errors are reported with a fixed text so internals do not leak.
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrBadKey):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrKeyMismatch):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, ErrInProgress):
		w.Header().Set("Retry-After", "1")
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
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
