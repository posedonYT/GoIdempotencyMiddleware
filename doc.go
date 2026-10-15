// Package idempotency provides net/http middleware that makes mutating
// requests idempotent.
//
// A client that did not receive a response (a timeout, a dropped connection)
// cannot know whether the server executed its POST. Retrying risks doing the
// work twice, for example charging a card twice. With this middleware the
// client sends an Idempotency-Key header with a unique value and may retry
// safely: the handler runs once and every retry gets the stored response.
//
// # Usage
//
//	store := idempotency.NewMemoryStore()
//	defer store.Close()
//
//	mw := idempotency.New(store)
//	http.ListenAndServe(":8080", mw(mux))
//
// New returns func(http.Handler) http.Handler, so it fits any router that
// supports standard middleware, such as chi and gorilla/mux.
//
// # Behavior
//
// For a request with a key, the middleware acts as follows:
//
//   - A new key: the handler runs and its response is stored.
//   - A repeat of a finished request: the stored response is replayed with the
//     header Idempotent-Replayed: true, and the handler is not called.
//   - The same key with a different method, URL or body: 422.
//   - The same key while the first request is still running: 409 with
//     Retry-After, or a wait for the result if WithWait is set.
//   - A 5xx response, a panic, or a response over the size limit is not stored
//     and the key is released, so the client may retry.
//
// Requests with other methods (by default only POST and PATCH are handled) and
// requests without a key (unless WithRequired is set) pass through unchanged.
//
// # Keys and scope
//
// A key is 1 to 255 printable ASCII characters. Keys are global by default;
// in a multi-user service use WithScope so that two users cannot collide.
//
// # Stores
//
// Records live in a Store. MemoryStore is included; it is fast and needs no
// setup, but its data is lost on restart and is not shared between processes.
// To run several instances, implement Store on top of a shared database. The
// optional Waiter interface enables WithWait.
//
// # Limits
//
// Streaming responses (SSE, WebSocket, Hijack) and trailers are not supported:
// do not put the middleware in front of such handlers.
package idempotency
