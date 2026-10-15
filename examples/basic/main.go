// Command basic is a minimal HTTP server protected by the idempotency
// middleware.
//
// Run it:
//
//	go run ./examples/basic
//
// Then send the same request twice:
//
//	curl -i -X POST localhost:8080/payments \
//	     -H 'Idempotency-Key: order-1001' -d '{"amount": 500}'
//	curl -i -X POST localhost:8080/payments \
//	     -H 'Idempotency-Key: order-1001' -d '{"amount": 500}'
//
// Both calls return the same payment_id, the second one with the header
// Idempotent-Replayed: true, and the server logs "charging" only once.
// Reusing the key with another body ('{"amount": 999}') gives 422.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/poseydon/idempotency"
)

// newHandler builds the application with the middleware in front of it.
func newHandler(store idempotency.Store) http.Handler {
	var lastID atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("POST /payments", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Amount int `json:"amount"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Amount <= 0 {
			http.Error(w, `{"error":"amount must be a positive number"}`, http.StatusBadRequest)
			return
		}

		id := lastID.Add(1)
		log.Printf("charging %d (payment %d)", req.Amount, id)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"payment_id": id, "amount": req.Amount})
	})

	return idempotency.New(store)(mux)
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	if err := run(*addr); err != nil {
		log.Fatal(err)
	}
}

func run(addr string) error {
	store := idempotency.NewMemoryStore(
		idempotency.WithTTL(24*time.Hour),
		idempotency.WithLockTimeout(30*time.Second),
	)
	defer func() { _ = store.Close() }()

	srv := &http.Server{
		Addr:              addr,
		Handler:           newHandler(store),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("listening on %s", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
