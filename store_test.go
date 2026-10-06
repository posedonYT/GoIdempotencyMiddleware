package idempotency

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrorsAreMatchable(t *testing.T) {
	sentinels := []struct {
		name string
		err  error
	}{
		{"ErrKeyMismatch", ErrKeyMismatch},
		{"ErrInProgress", ErrInProgress},
		{"ErrBadKey", ErrBadKey},
		{"ErrNotFound", ErrNotFound},
	}

	for i, tc := range sentinels {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := fmt.Errorf("op %q: %w", "k1", tc.err)

			// errors.Is(wrapped, tc.err) should be true
			if !errors.Is(wrapped, tc.err) {
				t.Errorf("errors.Is(%v, %s) = false, want true", wrapped, tc.name)
			}

			// For every OTHER sentinel error, it should be false
			for j, other := range sentinels {
				if i == j {
					continue
				}
				if errors.Is(wrapped, other.err) {
					t.Errorf("errors.Is(%v, %s) = true, want false", wrapped, other.name)
				}
			}
		})
	}
}

func TestStateString(t *testing.T) {
	tests := []struct {
		state    State
		expected string
	}{
		{StateInProgress, "in_progress"},
		{StateCompleted, "completed"},
		{State(0), "unknown"},
	}

	for _, tc := range tests {
		got := tc.state.String()
		if got != tc.expected {
			t.Errorf("State(%d).String() = %q, want %q", tc.state, got, tc.expected)
		}
	}
}
