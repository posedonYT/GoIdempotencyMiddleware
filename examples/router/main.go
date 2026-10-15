// Command router shows how to plug the middleware into a router and which
// options matter in a real service.
//
// The middleware has the standard shape func(http.Handler) http.Handler, so it
// works with any router that accepts such middleware without an adapter:
//
//	// chi
//	r := chi.NewRouter()
//	r.Use(idem)
//
//	// gorilla/mux (mux.MiddlewareFunc has the same underlying type)
//	r := mux.NewRouter()
//	r.Use(mux.MiddlewareFunc(idem))
//
//	// plain net/http
//	http.ListenAndServe(addr, idem(mux))
//
// This example uses only the standard library: the middleware wraps the /api
// subtree and leaves /health alone.
//
//	go run ./examples/router
//
//	curl -i -X POST localhost:8080/api/orders -d 'tea'                          # 400: key is required
//	curl -i -X POST localhost:8080/api/orders -H 'Idempotency-Key: k1' -d 'tea' # 201
//	curl -i -X POST localhost:8080/api/orders -H 'Idempotency-Key: k1' -d 'tea' # 201, replayed
//	curl -i localhost:8080/health                                               # no key needed
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/poseydon/idempotency"
)

func newHandler(store idempotency.Store) http.Handler {
	var lastID atomic.Int64

	api := http.NewServeMux()
	api.HandleFunc("POST /orders", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, "order %d\n", lastID.Add(1))
	})
	api.HandleFunc("PUT /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "order %s updated\n", r.PathValue("id"))
	})

	idem := idempotency.New(store,
		// Make the header mandatory for the API.
		idempotency.WithRequired(true),
		// PUT is idempotent by definition, but a retried PUT that triggers side
		// effects (an email, a charge) benefits from the same protection.
		idempotency.WithMethods(http.MethodPost, http.MethodPut, http.MethodPatch),
		// A parallel retry waits for the first request instead of getting 409.
		idempotency.WithWait(5*time.Second),
		// Do not cache 5xx and also let the client retry a 429.
		idempotency.WithCacheable(func(status int) bool {
			return status < 500 && status != http.StatusTooManyRequests
		}),
		// Our own error format.
		idempotency.WithErrorHandler(jsonProblem),
	)

	root := http.NewServeMux()
	root.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "ok")
	})
	root.Handle("/api/", http.StripPrefix("/api", idem(api)))
	return root
}

// jsonProblem renders middleware errors in the service's own error format.
func jsonProblem(w http.ResponseWriter, _ *http.Request, err error) {
	status, code := http.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, idempotency.ErrBadKey):
		status, code = http.StatusBadRequest, "bad_idempotency_key"
	case errors.Is(err, idempotency.ErrBodyTooLarge):
		status, code = http.StatusRequestEntityTooLarge, "body_too_large"
	case errors.Is(err, idempotency.ErrKeyMismatch):
		status, code = http.StatusUnprocessableEntity, "idempotency_key_reused"
	case errors.Is(err, idempotency.ErrInProgress):
		status, code = http.StatusConflict, "request_in_progress"
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	store := idempotency.NewMemoryStore()
	defer func() { _ = store.Close() }()

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           newHandler(store),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("listening on %s", srv.Addr)
	return srv.ListenAndServe()
}
