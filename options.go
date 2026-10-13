package idempotency

import (
	"net/http"
	"strings"
)

const (
	defaultHeader          = "Idempotency-Key"
	defaultMaxRequestBody  = 1 << 20
	defaultMaxResponseBody = 1 << 20
)

type config struct {
	header          string
	methods         map[string]bool
	required        bool
	maxRequestBody  int64
	maxResponseBody int
	scope           func(*http.Request) string
}

func defaultConfig() config {
	return config{
		header:          defaultHeader,
		methods:         map[string]bool{http.MethodPost: true, http.MethodPatch: true},
		maxRequestBody:  defaultMaxRequestBody,
		maxResponseBody: defaultMaxResponseBody,
	}
}

// Option configures the middleware.
type Option func(*config)

// WithHeader sets the request header that carries the idempotency key.
// The default is "Idempotency-Key".
func WithHeader(name string) Option {
	return func(c *config) {
		c.header = name
	}
}

// WithMethods replaces the set of HTTP methods the middleware applies to.
// Method names are upper-cased. The default is POST and PATCH.
func WithMethods(methods ...string) Option {
	return func(c *config) {
		c.methods = make(map[string]bool, len(methods))
		for _, m := range methods {
			c.methods[strings.ToUpper(m)] = true
		}
	}
}

// WithRequired makes a missing idempotency key an error instead of letting the
// request through. The default is false.
func WithRequired(v bool) Option {
	return func(c *config) {
		c.required = v
	}
}

// WithMaxRequestBody sets the maximum request body size in bytes.
// The default is 1 MiB.
func WithMaxRequestBody(n int64) Option {
	return func(c *config) {
		c.maxRequestBody = n
	}
}

// WithMaxResponseBody sets the maximum response body size in bytes that is
// recorded for replay. The default is 1 MiB.
func WithMaxResponseBody(n int) Option {
	return func(c *config) {
		c.maxResponseBody = n
	}
}

// WithScope sets a function that returns the scope (for example, a user ID)
// prepended to the key, so that different clients never share a key.
// By default the scope is empty.
func WithScope(f func(*http.Request) string) Option {
	return func(c *config) {
		c.scope = f
	}
}
