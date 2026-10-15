// Command scope shows how to isolate idempotency keys between users.
//
// Without a scope the key is global: if two users happen to send the same
// Idempotency-Key, the second one would get a 422 or, worse, the first user's
// response. WithScope puts the user into the store key, so every user has
// their own key space.
//
//	go run ./examples/scope
//
//	curl -i -X POST localhost:8080/orders -H 'X-User-Id: alice' -H 'Idempotency-Key: k1' -d 'tea'
//	curl -i -X POST localhost:8080/orders -H 'X-User-Id: bob'   -H 'Idempotency-Key: k1' -d 'tea'
//
// Alice and Bob each create their own order even though the key is the same.
//
// The user id here comes from a plain header to keep the example short. In a
// real service take it from verified authentication (a session or a validated
// token), never from something the client can freely choose.
package main

import (
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/poseydon/idempotency"
)

const userHeader = "X-User-Id"

func newHandler(store idempotency.Store) http.Handler {
	var lastID atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		id := lastID.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, "order %d for %s\n", id, r.Header.Get(userHeader))
	})

	withIdempotency := idempotency.New(store,
		idempotency.WithRequired(true),
		idempotency.WithScope(func(r *http.Request) string {
			return r.Header.Get(userHeader)
		}),
	)

	// Reject anonymous requests before the middleware: an empty scope would
	// put all of them into one shared key space.
	return requireUser(withIdempotency(mux))
}

func requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(userHeader) == "" {
			http.Error(w, "missing "+userHeader, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
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
