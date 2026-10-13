package idempotency

import (
	"errors"
	"fmt"
	"net/http"
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
			// placeholder until the store is wired in
			next.ServeHTTP(w, r)
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
