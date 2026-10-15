package idempotency

import (
	"net/http"
	"strings"
	"time"
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
	cacheable       func(status int) bool
	failOpen        bool
	errorHandler    func(w http.ResponseWriter, r *http.Request, err error)
	wait            time.Duration
}

func defaultConfig() config {
	return config{
		header:          defaultHeader,
		methods:         map[string]bool{http.MethodPost: true, http.MethodPatch: true},
		maxRequestBody:  defaultMaxRequestBody,
		maxResponseBody: defaultMaxResponseBody,
		cacheable:       defaultCacheable,
		errorHandler:    defaultErrorHandler,
	}
}

// validate panics if the configuration cannot work. Bad options are a
// programming error, so they fail at construction rather than per request.
func (c *config) validate() {
	switch {
	case c.header == "":
		panic("idempotency: WithHeader: header name must not be empty")
	case c.maxRequestBody <= 0:
		panic("idempotency: WithMaxRequestBody: limit must be positive")
	case c.maxResponseBody <= 0:
		panic("idempotency: WithMaxResponseBody: limit must be positive")
	case c.cacheable == nil:
		panic("idempotency: WithCacheable: func must not be nil")
	case c.errorHandler == nil:
		panic("idempotency: WithErrorHandler: func must not be nil")
	case c.wait < 0:
		panic("idempotency: WithWait: duration must not be negative")
	}
}

// defaultCacheable stores every response except server errors: a 5xx is
// usually a transient failure, and the client must be able to retry it.
func defaultCacheable(status int) bool {
	return status < http.StatusInternalServerError
}

// Option configures the middleware.
type Option func(*config)

// WithHeader sets the request header that carries the idempotency key.
// The default is "Idempotency-Key".
// The name is canonicalized, so "idempotency-key" and "Idempotency-Key" are
// the same header. An empty name makes New panic.
func WithHeader(name string) Option {
	return func(c *config) {
		c.header = http.CanonicalHeaderKey(name)
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

// WithCacheable decides which responses are stored for replay. A response for
// which f returns false is delivered to the client but not stored, and the key
// is released so the client can retry. The default stores everything below 500.
func WithCacheable(f func(status int) bool) Option {
	return func(c *config) {
		c.cacheable = f
	}
}

// WithFailOpen chooses what happens when the Store fails. By default the
// middleware fails closed and answers 500, which is the safe choice for
// operations like payments. With failOpen set, the request is passed to the
// handler without idempotency protection.
func WithFailOpen(failOpen bool) Option {
	return func(c *config) {
		c.failOpen = failOpen
	}
}

// WithErrorHandler replaces the response written for errors produced by the
// middleware itself (bad key, key reuse, request in progress, ...). err wraps
// one of the package's Err* sentinels, so use errors.Is to pick a response.
// The default writes a JSON body {"error": "..."}.
func WithErrorHandler(f func(w http.ResponseWriter, r *http.Request, err error)) Option {
	return func(c *config) {
		c.errorHandler = f
	}
}

// WithWait makes a request whose key is still in progress wait up to d for the
// first request to finish and then replay its response, instead of failing
// with 409 Conflict. It needs a Store that implements Waiter; with any other
// Store it has no effect. Zero (the default) disables waiting.
func WithWait(d time.Duration) Option {
	return func(c *config) {
		c.wait = d
	}
}
