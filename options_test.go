package idempotency

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
)

// apply builds a config from the defaults and the given options.
func apply(opts ...Option) config {
	c := defaultConfig()
	for _, opt := range opts {
		opt(&c)
	}
	return c
}

func TestDefaultConfig(t *testing.T) {
	c := defaultConfig()

	if c.header != "Idempotency-Key" {
		t.Errorf("header = %q, want %q", c.header, "Idempotency-Key")
	}
	if c.header != http.CanonicalHeaderKey(c.header) {
		t.Errorf("header %q is not canonical, direct map lookups in r.Header would miss it", c.header)
	}
	wantMethods := map[string]bool{http.MethodPost: true, http.MethodPatch: true}
	if !maps.Equal(c.methods, wantMethods) {
		t.Errorf("methods = %v, want %v", c.methods, wantMethods)
	}
	if c.required {
		t.Error("required = true, want false")
	}
	if c.maxRequestBody != 1<<20 {
		t.Errorf("maxRequestBody = %d, want %d", c.maxRequestBody, 1<<20)
	}
	if c.maxResponseBody != 1<<20 {
		t.Errorf("maxResponseBody = %d, want %d", c.maxResponseBody, 1<<20)
	}
	if c.scope != nil {
		t.Error("scope != nil, want nil")
	}
}

func TestDefaultConfigIsIndependent(t *testing.T) {
	// Each call must return its own methods map, otherwise an option applied
	// to one middleware would leak into every other one.
	a, b := defaultConfig(), defaultConfig()
	a.methods[http.MethodPut] = true
	if b.methods[http.MethodPut] {
		t.Error("defaultConfig() shares the methods map between calls")
	}
}

func TestWithHeader(t *testing.T) {
	c := apply(WithHeader("X-Request-Id"))
	if c.header != "X-Request-Id" {
		t.Errorf("header = %q, want %q", c.header, "X-Request-Id")
	}
}

func TestWithMethods(t *testing.T) {
	tests := []struct {
		name    string
		methods []string
		want    map[string]bool
	}{
		{
			name:    "replaces defaults",
			methods: []string{http.MethodPut},
			want:    map[string]bool{http.MethodPut: true},
		},
		{
			name:    "several methods",
			methods: []string{http.MethodPost, http.MethodPut, http.MethodDelete},
			want:    map[string]bool{http.MethodPost: true, http.MethodPut: true, http.MethodDelete: true},
		},
		{
			name:    "upper-cased",
			methods: []string{"post", "Patch"},
			want:    map[string]bool{http.MethodPost: true, http.MethodPatch: true},
		},
		{
			name:    "duplicates collapse",
			methods: []string{"POST", "post"},
			want:    map[string]bool{http.MethodPost: true},
		},
		{
			name:    "no methods gives an empty set",
			methods: nil,
			want:    map[string]bool{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := apply(WithMethods(tc.methods...))
			if !maps.Equal(c.methods, tc.want) {
				t.Errorf("methods = %v, want %v", c.methods, tc.want)
			}
		})
	}
}

func TestWithRequired(t *testing.T) {
	if c := apply(WithRequired(true)); !c.required {
		t.Error("WithRequired(true): required = false, want true")
	}
	if c := apply(WithRequired(true), WithRequired(false)); c.required {
		t.Error("WithRequired(false) after true: required = true, want false")
	}
}

func TestWithMaxRequestBody(t *testing.T) {
	c := apply(WithMaxRequestBody(512))
	if c.maxRequestBody != 512 {
		t.Errorf("maxRequestBody = %d, want 512", c.maxRequestBody)
	}
	if c.maxResponseBody != defaultMaxResponseBody {
		t.Errorf("maxResponseBody = %d, changed by WithMaxRequestBody", c.maxResponseBody)
	}
}

func TestWithMaxResponseBody(t *testing.T) {
	c := apply(WithMaxResponseBody(2048))
	if c.maxResponseBody != 2048 {
		t.Errorf("maxResponseBody = %d, want 2048", c.maxResponseBody)
	}
	if c.maxRequestBody != defaultMaxRequestBody {
		t.Errorf("maxRequestBody = %d, changed by WithMaxResponseBody", c.maxRequestBody)
	}
}

func TestWithScope(t *testing.T) {
	c := apply(WithScope(func(r *http.Request) string {
		return r.Header.Get("X-User-Id")
	}))
	if c.scope == nil {
		t.Fatal("scope = nil, want the given function")
	}

	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-User-Id", "42")
	if got := c.scope(r); got != "42" {
		t.Errorf("scope(r) = %q, want %q", got, "42")
	}
}

func TestOptionsAreIndependent(t *testing.T) {
	// An option changes only its own field.
	c := apply(WithHeader("X-Key"))
	want := defaultConfig()
	want.header = "X-Key"

	if c.header != want.header ||
		!maps.Equal(c.methods, want.methods) ||
		c.required != want.required ||
		c.maxRequestBody != want.maxRequestBody ||
		c.maxResponseBody != want.maxResponseBody ||
		c.scope != nil {
		t.Errorf("WithHeader changed other fields: got %+v", c)
	}
}

func TestOptionsLastWins(t *testing.T) {
	c := apply(WithHeader("A"), WithHeader("B"))
	if c.header != "B" {
		t.Errorf("header = %q, want the last option %q", c.header, "B")
	}
}
