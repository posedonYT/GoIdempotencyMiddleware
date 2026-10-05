package idempotency

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newBodyRequest(body string) (*httptest.ResponseRecorder, *http.Request) {
	return httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
}

func TestReadBody_ReturnsBodyUnchanged(t *testing.T) {
	w, r := newBodyRequest(`{"amount":100}`)

	got, err := readBody(w, r, 1024)
	if err != nil {
		t.Fatalf("readBody: unexpected error: %v", err)
	}
	if string(got) != `{"amount":100}` {
		t.Fatalf("got %q, want %q", got, `{"amount":100}`)
	}
}

func TestReadBody_EmptyBody(t *testing.T) {
	w, r := newBodyRequest("")

	got, err := readBody(w, r, 1024)
	if err != nil {
		t.Fatalf("readBody: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestReadBody_BodyReadableAgain(t *testing.T) {
	const payload = "hello, idempotency"
	w, r := newBodyRequest(payload)

	first, err := readBody(w, r, 1024)
	if err != nil {
		t.Fatalf("readBody: unexpected error: %v", err)
	}

	second, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("re-read r.Body: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("re-read body %q differs from first read %q", second, first)
	}
	if string(second) != payload {
		t.Fatalf("re-read body %q, want %q", second, payload)
	}
}

func TestReadBody_BodyEqualToLimit(t *testing.T) {
	const limit = 16
	payload := strings.Repeat("a", limit)
	w, r := newBodyRequest(payload)

	got, err := readBody(w, r, limit)
	if err != nil {
		t.Fatalf("readBody: unexpected error at exactly the limit: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("got %q, want %q", got, payload)
	}
}

func TestReadBody_BodyOverLimit(t *testing.T) {
	const limit = 16
	w, r := newBodyRequest(strings.Repeat("a", limit+1))

	got, err := readBody(w, r, limit)
	if err == nil {
		t.Fatal("readBody: expected error for body over the limit, got nil")
	}
	if got != nil {
		t.Fatalf("got %q, want nil data on error", got)
	}

	var tooBig *http.MaxBytesError
	if !errors.As(err, &tooBig) {
		t.Fatalf("errors.As(*http.MaxBytesError) = false for %v", err)
	}
	if tooBig.Limit != limit {
		t.Fatalf("MaxBytesError.Limit = %d, want %d", tooBig.Limit, limit)
	}
}

func TestValidateKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"simple", "abc123", false},
		{"uuid", "123e4567-e89b-12d3-a456-426614174000", false},
		{"printable punctuation", "!~_-.:/", false},
		{"first printable char", "!", false},
		{"last printable char", "~", false},
		{"max length", strings.Repeat("a", maxKeyLen), false},
		{"empty", "", true},
		{"too long", strings.Repeat("a", maxKeyLen+1), true},
		{"space", "a b", true},
		{"tab", "a\tb", true},
		{"newline", "a\nb", true},
		{"null byte", "a\x00b", true},
		{"DEL", "a\x7fb", true},
		{"non-ASCII", "ключ", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateKey(tt.key)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("validateKey(%q): unexpected error: %v", tt.key, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateKey(%q): expected error, got nil", tt.key)
			}
			if !errors.Is(err, ErrBadKey) {
				t.Fatalf("validateKey(%q): error %v does not wrap ErrBadKey", tt.key, err)
			}
		})
	}
}

func TestStoreKey(t *testing.T) {
	t.Run("joins scope and key with NUL", func(t *testing.T) {
		if got, want := storeKey("user-1", "k1"), "user-1\x00k1"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("deterministic", func(t *testing.T) {
		if storeKey("s", "k") != storeKey("s", "k") {
			t.Fatal("storeKey is not deterministic")
		}
	})

	t.Run("different scopes do not collide", func(t *testing.T) {
		if storeKey("a", "k") == storeKey("b", "k") {
			t.Fatal("same key in different scopes produced equal store keys")
		}
	})

	t.Run("separator prevents boundary ambiguity", func(t *testing.T) {
		if storeKey("ab", "c") == storeKey("a", "bc") {
			t.Fatal(`("ab","c") and ("a","bc") produced equal store keys`)
		}
	})

	t.Run("empty scope", func(t *testing.T) {
		if got, want := storeKey("", "k"), "\x00k"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

type fpInput struct {
	method string
	target string
	body   string
	header map[string]string
}

func (in fpInput) fingerprint() string {
	r := httptest.NewRequest(in.method, in.target, nil)
	for k, v := range in.header {
		r.Header.Set(k, v)
	}
	return fingerprint(r, []byte(in.body))
}

func TestFingerprint(t *testing.T) {
	base := fpInput{method: http.MethodPost, target: "/pay?x=1", body: "body"}

	with := func(f func(in *fpInput)) fpInput {
		in := base
		f(&in)
		return in
	}

	tests := []struct {
		name      string
		a, b      fpInput
		wantEqual bool
	}{
		{
			name:      "identical requests",
			a:         base,
			b:         base,
			wantEqual: true,
		},
		{
			name:      "empty body equals empty body",
			a:         with(func(in *fpInput) { in.body = "" }),
			b:         with(func(in *fpInput) { in.body = "" }),
			wantEqual: true,
		},
		{
			name:      "headers are ignored",
			a:         base,
			b:         with(func(in *fpInput) { in.header = map[string]string{"Idempotency-Key": "abc", "User-Agent": "x"} }),
			wantEqual: true,
		},
		{
			name: "different method",
			a:    base,
			b:    with(func(in *fpInput) { in.method = http.MethodPut }),
		},
		{
			name: "different path",
			a:    base,
			b:    with(func(in *fpInput) { in.target = "/refund?x=1" }),
		},
		{
			name: "different query value",
			a:    base,
			b:    with(func(in *fpInput) { in.target = "/pay?x=2" }),
		},
		{
			name: "query present vs missing",
			a:    base,
			b:    with(func(in *fpInput) { in.target = "/pay" }),
		},
		{
			name: "different body",
			a:    base,
			b:    with(func(in *fpInput) { in.body = "other" }),
		},
		{
			name: "body vs empty body",
			a:    base,
			b:    with(func(in *fpInput) { in.body = "" }),
		},
		{
			name: "path/body boundary is unambiguous",
			a:    fpInput{method: http.MethodPost, target: "/a", body: "bc"},
			b:    fpInput{method: http.MethodPost, target: "/ab", body: "c"},
		},
		{
			name: "method/path boundary is unambiguous",
			a:    fpInput{method: "POS", target: "/T", body: ""},
			b:    fpInput{method: "POST", target: "/", body: ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := tt.a.fingerprint(), tt.b.fingerprint()

			if tt.wantEqual && a != b {
				t.Fatalf("fingerprints differ, want equal:\n a=%s\n b=%s", a, b)
			}
			if !tt.wantEqual && a == b {
				t.Fatalf("fingerprints equal, want different: %s", a)
			}
		})
	}
}

func TestFingerprint_NilAndEmptyBodyEqual(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/pay", nil)

	if fingerprint(r, nil) != fingerprint(r, []byte{}) {
		t.Fatal("nil and empty body must produce the same fingerprint")
	}
}

func TestFingerprint_Format(t *testing.T) {
	got := fingerprint(httptest.NewRequest(http.MethodPost, "/pay", nil), []byte("x"))

	if len(got) != 64 {
		t.Fatalf("len = %d, want 64 (hex sha256): %q", len(got), got)
	}
	if strings.Trim(got, "0123456789abcdef") != "" {
		t.Fatalf("not lowercase hex: %q", got)
	}
}

func TestFingerprint_DoesNotConsumeBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/pay", strings.NewReader("payload"))

	_ = fingerprint(r, []byte("payload"))

	got, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read r.Body: %v", err)
	}
	if string(got) != "payload" {
		t.Fatalf("r.Body = %q, want %q", got, "payload")
	}
}

func TestFingerprint_GoldenValues(t *testing.T) {
	tests := []struct {
		name   string
		method string
		target string
		body   string
		want   string
	}{
		{
			name:   "post with query and body",
			method: http.MethodPost,
			target: "/pay?x=1",
			body:   `{"amount":100}`,
			want:   "c2bc37f93a798e13c339301b166b731ab34da6fdbaa830eb220026e3247b6768",
		},
		{
			name:   "get without body",
			method: http.MethodGet,
			target: "/",
			body:   "",
			want:   "139fb16fdd19069e75f6f3dd0e619a89fb157dab65e8722c66e6b3fd43f7900c",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.target, nil)

			got := fingerprint(r, []byte(tt.body))
			if got != tt.want {
				t.Fatalf("fingerprint format changed: stored entries would no longer match\n got:  %s\n want: %s", got, tt.want)
			}
		})
	}
}
