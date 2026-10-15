package idempotency

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func FuzzValidateKey(f *testing.F) {
	for _, seed := range []string{"", "k1", "a b", strings.Repeat("a", maxKeyLen), strings.Repeat("a", maxKeyLen+1), "ключ", "a\x00b", "\x7f"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, key string) {
		err := validateKey(key) // must never panic
		if err != nil {
			return
		}
		// An accepted key is safe to put into a store key and a header.
		if key == "" || len(key) > maxKeyLen {
			t.Fatalf("accepted key of length %d", len(key))
		}
		for i := 0; i < len(key); i++ {
			if key[i] < 0x21 || key[i] > 0x7E {
				t.Fatalf("accepted key %q with byte 0x%02x at %d", key, key[i], i)
			}
		}
	})
}

func FuzzFingerprint(f *testing.F) {
	f.Add("POST", "/orders?x=1", []byte(`{"a":1}`))
	f.Add("PATCH", "/", []byte(nil))

	f.Fuzz(func(t *testing.T, method, target string, body []byte) {
		r, err := http.NewRequest(method, "http://example.com/", nil)
		if err != nil {
			t.Skip()
		}
		r.URL.Path, r.URL.RawQuery = target, ""

		a, b := fingerprint(r, body), fingerprint(r, body)
		if a != b || len(a) != 64 {
			t.Fatalf("fingerprint is not deterministic or not a sha256 hex: %q, %q", a, b)
		}
	})
}

func FuzzStoreKeyIsInjective(f *testing.F) {
	f.Add("alice", "k1", "bob", "k1")

	f.Fuzz(func(t *testing.T, scope1, key1, scope2, key2 string) {
		// Keys are validated, so only valid keys can reach storeKey.
		if validateKey(key1) != nil || validateKey(key2) != nil {
			t.Skip()
		}
		same := storeKey(scope1, key1) == storeKey(scope2, key2)
		if same != (scope1 == scope2 && key1 == key2) {
			t.Fatalf("storeKey(%q,%q) vs storeKey(%q,%q): collision = %v", scope1, key1, scope2, key2, same)
		}
	})
}

func newBenchHandler(b *testing.B, opts ...Option) http.Handler {
	b.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	})
	return New(NewMemoryStore(WithCleanupInterval(0)), opts...)(next)
}

func BenchmarkMiddlewareReplay(b *testing.B) {
	h := newBenchHandler(b)
	body := strings.Repeat("x", 512)
	do(h, http.MethodPost, "/orders", "k1", body) // the first request stores the response

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		r := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body))
		r.Header.Set("Idempotency-Key", "k1")
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
}

func BenchmarkMiddlewareNewKey(b *testing.B) {
	h := newBenchHandler(b)
	body := strings.Repeat("x", 512)
	keys := make([]string, b.N)
	for i := range keys {
		keys[i] = "key-" + strconv.Itoa(i)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		r := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body))
		r.Header.Set("Idempotency-Key", keys[i])
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
}

func BenchmarkMiddlewarePassThrough(b *testing.B) {
	h := newBenchHandler(b)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		r := httptest.NewRequest(http.MethodGet, "/orders", nil)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
}

func BenchmarkMemoryStoreAcquire(b *testing.B) {
	s := NewMemoryStore(WithCleanupInterval(0))
	ctx := b.Context()
	_, _, _ = s.Acquire(ctx, "k", "fp")

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _, _ = s.Acquire(ctx, "k", "fp")
		}
	})
}
