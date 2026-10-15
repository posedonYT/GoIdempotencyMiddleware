package main

import (
	"errors"
	"testing"

	"github.com/poseydon/idempotency"
)

func TestSecondRequestIsReplayed(t *testing.T) {
	h := newHandler(newLoggingStore())

	first, second := send(h, "k1"), send(h, "k1")
	if first.Code != 201 || second.Code != 201 {
		t.Fatalf("statuses = %d, %d; want 201, 201", first.Code, second.Code)
	}
	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Error("the second response is not marked as replayed")
	}
}

func TestStoreContract(t *testing.T) {
	s := newLoggingStore()
	ctx := t.Context()

	if _, acquired, _ := s.Acquire(ctx, "k", "fp"); !acquired {
		t.Fatal("first Acquire did not acquire")
	}
	if _, acquired, _ := s.Acquire(ctx, "k", "fp"); acquired {
		t.Error("second Acquire acquired the same key")
	}
	if err := s.Complete(ctx, "missing", idempotency.Response{}); !errors.Is(err, idempotency.ErrNotFound) {
		t.Errorf("Complete on a missing key = %v, want ErrNotFound", err)
	}
	if err := s.Release(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, acquired, _ := s.Acquire(ctx, "k", "fp"); !acquired {
		t.Error("the key was not freed by Release")
	}
}
