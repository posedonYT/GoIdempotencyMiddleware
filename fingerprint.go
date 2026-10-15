package idempotency

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const maxKeyLen = 255

func validateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty", ErrBadKey)
	}
	if len(key) > maxKeyLen {
		return fmt.Errorf("%w: too long", ErrBadKey)
	}
	for i := 0; i < len(key); i++ {
		b := key[i]
		if b < 0x21 || b > 0x7E {
			return fmt.Errorf("%w: invalid character %q at position %d", ErrBadKey, b, i)
		}
	}
	return nil
}

func storeKey(scope, key string) string {
	return scope + "\x00" + key
}

func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, fmt.Errorf("%w (limit is %d bytes): %w", ErrBodyTooLarge, tooLarge.Limit, err)
		}
		return nil, fmt.Errorf("%w: %w", ErrBadBody, err)
	}

	r.Body = io.NopCloser(bytes.NewReader(data))

	return data, nil
}

func fingerprint(r *http.Request, body []byte) string {
	h := sha256.New()
	// hash.Hash writes never return an error.
	_, _ = io.WriteString(h, r.Method)
	_, _ = io.WriteString(h, "\n")
	_, _ = io.WriteString(h, r.URL.RequestURI())
	_, _ = io.WriteString(h, "\n")
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}
