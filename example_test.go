package idempotency_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/poseydon/idempotency"
)

// post sends a POST with the given key and body through h.
func post(h http.Handler, key, body string, extra ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/payments", strings.NewReader(body))
	r.Header.Set("Idempotency-Key", key)
	for i := 0; i+1 < len(extra); i += 2 {
		r.Header.Set(extra[i], extra[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func ExampleNew() {
	store := idempotency.NewMemoryStore()
	defer func() { _ = store.Close() }()

	charges := 0
	handler := idempotency.New(store)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		charges++
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, "payment created")
	}))

	first := post(handler, "order-1001", `{"amount":500}`)
	retry := post(handler, "order-1001", `{"amount":500}`)

	fmt.Println(first.Code, first.Body)
	fmt.Println(retry.Code, retry.Body, retry.Header().Get("Idempotent-Replayed"))
	fmt.Println("charges:", charges)
	// Output:
	// 201 payment created
	// 201 payment created true
	// charges: 1
}

func ExampleNew_keyReuse() {
	store := idempotency.NewMemoryStore()
	defer func() { _ = store.Close() }()

	handler := idempotency.New(store)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))

	post(handler, "order-1001", `{"amount":500}`)
	// The same key, but another body: this is a client bug, not a retry.
	w := post(handler, "order-1001", `{"amount":999}`)

	fmt.Println(w.Code)
	// Output: 422
}

func ExampleWithScope() {
	store := idempotency.NewMemoryStore()
	defer func() { _ = store.Close() }()

	orders := 0
	handler := idempotency.New(store, idempotency.WithScope(func(r *http.Request) string {
		return r.Header.Get("X-User-Id")
	}))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		orders++
		w.WriteHeader(http.StatusCreated)
	}))

	// The same key from two users is two different operations.
	post(handler, "k1", "tea", "X-User-Id", "alice")
	post(handler, "k1", "tea", "X-User-Id", "bob")
	post(handler, "k1", "tea", "X-User-Id", "alice") // a retry

	fmt.Println("orders:", orders)
	// Output: orders: 2
}

func ExampleWithRequired() {
	store := idempotency.NewMemoryStore()
	defer func() { _ = store.Close() }()

	handler := idempotency.New(store, idempotency.WithRequired(true))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	r := httptest.NewRequest(http.MethodPost, "/payments", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	fmt.Println(w.Code)
	// Output: 400
}

func ExampleWithErrorHandler() {
	store := idempotency.NewMemoryStore()
	defer func() { _ = store.Close() }()

	handler := idempotency.New(store, idempotency.WithErrorHandler(
		func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.Is(err, idempotency.ErrKeyMismatch) {
				http.Error(w, "this key was used for another request", http.StatusUnprocessableEntity)
				return
			}
			http.Error(w, "bad request", http.StatusBadRequest)
		}))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	post(handler, "k1", "a")
	w := post(handler, "k1", "b")

	fmt.Print(w.Body)
	// Output: this key was used for another request
}

func ExampleNewMemoryStore() {
	store := idempotency.NewMemoryStore(
		idempotency.WithTTL(time.Hour),              // how long a response can be replayed
		idempotency.WithLockTimeout(10*time.Second), // how long a running request holds its key
		idempotency.WithMaxEntries(100_000),         // protection from unbounded growth
	)
	defer func() { _ = store.Close() }() // stops the background cleanup

	_ = idempotency.New(store)
	fmt.Println("ready")
	// Output: ready
}
