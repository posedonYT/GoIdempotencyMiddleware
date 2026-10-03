package idempotency

import (
	"context"
	"errors"
	"net/http"
)

// State is the lifecycle of an idempotency record.
type State int

const (
	// StateInProgress means a request holds the key and has not finished yet.
	StateInProgress State = iota + 1
	// StateCompleted means a response is stored and can be replayed.
	StateCompleted
)

// String returns a stable label for s.
func (s State) String() string {
	switch s {
	case StateInProgress:
		return "in_progress"
	case StateCompleted:
		return "completed"
	default:
		return "unknown"
	}
}

// Response is a stored HTTP response that can be replayed.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Entry describes the current state of a key in a Store.
type Entry struct {
	State       State
	Fingerprint string
	Response    *Response
}

// Store persists idempotency records and serializes concurrent use of one key.
type Store interface {
	// Acquire atomically inserts an in-progress record or returns the existing one.
	// acquired is true only when this call created the record.
	Acquire(ctx context.Context, key, fingerprint string) (e Entry, acquired bool, err error)
	// Complete stores resp and moves the record to StateCompleted.
	Complete(ctx context.Context, key string, resp Response) error
	// Release deletes an in-progress record so a later request can Acquire the key again.
	Release(ctx context.Context, key string) error
}

var (
	// ErrKeyMismatch is returned when a key is reused with a different request.
	ErrKeyMismatch = errors.New("idempotency: key reused with a different request")
	// ErrInProgress is returned when a request with this key is still running.
	ErrInProgress = errors.New("idempotency: request is still in progress")
	// ErrBadKey is returned when an idempotency key fails validation.
	ErrBadKey = errors.New("idempotency: bad key")
)
