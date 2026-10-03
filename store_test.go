package idempotency

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrorsAreMatchable(t *testing.T) {
	wrapped := fmt.Errorf("acquire %q: %w", "k1", ErrBadKey)

	// Проверяем, что wrapped "является" ErrBadKey
	if !errors.Is(wrapped, ErrBadKey) {
		t.Errorf("wrapped error should be ErrBadKey")
	}

	// Проверяем, что wrapped НЕ является ErrInProgress
	if errors.Is(wrapped, ErrInProgress) {
		t.Errorf("wrapped error should not be ErrInProgress")
	}
}
