package idempotency

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
		return nil, fmt.Errorf("read request body %w", err)
	}

	r.Body = io.NopCloser(bytes.NewReader(data))

	return data, nil
}

func fingerprint(r *http.Request, body []byte) string {
	h := sha256.New()
	io.WriteString(h, r.Method)
	io.WriteString(h, "\n")
	io.WriteString(h, r.URL.RequestURI())
	io.WriteString(h, "\n")
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}
