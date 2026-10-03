package idempotency

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestRecorder(t *testing.T) {
	tests := []struct {
		name       string
		run        func(w http.ResponseWriter)
		wantStatus int
		wantBody   string
		wantHeader http.Header
		expectOK   bool
		limit      int
	}{
		{
			name:       "write only",
			run:        func(w http.ResponseWriter) { w.Write([]byte("hi")) },
			wantStatus: 200,
			wantBody:   "hi",
			wantHeader: http.Header{},
			expectOK:   true,
			limit:      1024,
		},
		{
			name: "WriteHeader(201) + Write",
			run: func(w http.ResponseWriter) {
				w.WriteHeader(201)
				w.Write([]byte("abc"))
			},
			wantStatus: 201,
			wantBody:   "abc",
			wantHeader: http.Header{},
			expectOK:   true,
			limit:      1024,
		},
		{
			name: "WriteHeader twice, only the first matters",
			run: func(w http.ResponseWriter) {
				w.WriteHeader(201)
				w.WriteHeader(500)
				w.Write([]byte("a"))
			},
			wantStatus: 201,
			wantBody:   "a",
			wantHeader: http.Header{},
			expectOK:   true,
			limit:      1024,
		},
		{
			name: "Header set after WriteHeader",
			run: func(w http.ResponseWriter) {
				w.WriteHeader(202)
				w.Header().Set("X-Hi", "1")
				w.Write([]byte("foo"))
			},
			wantStatus: 202,
			wantBody:   "foo",
			wantHeader: http.Header{},
			expectOK:   true,
			limit:      1024,
		},
		{
			name: "Header set before WriteHeader",
			run: func(w http.ResponseWriter) {
				w.Header().Set("X-Pre", "5")
				w.WriteHeader(407)
				w.Write([]byte("bar"))
			},
			wantStatus: 407,
			wantBody:   "bar",
			wantHeader: http.Header{"X-Pre": []string{"5"}},
			expectOK:   true,
			limit:      1024,
		},
		{
			name: "No WriteHeader, only Header set before Write",
			run: func(w http.ResponseWriter) {
				w.Header().Set("A", "X")
				w.Write([]byte("b"))
			},
			wantStatus: 200,
			wantBody:   "b",
			wantHeader: http.Header{"A": []string{"X"}},
			expectOK:   true,
			limit:      1024,
		},
		{
			name: "Body exceeds limit",
			run: func(w http.ResponseWriter) {
				w.Write([]byte("12345"))
				w.Write([]byte("67890")) // total 10 for limit=8
			},
			wantStatus: 200,
			wantBody:   "",
			wantHeader: http.Header{},
			expectOK:   false,
			limit:      8,
		},
		{
			name: "Body size equals limit",
			run: func(w http.ResponseWriter) {
				w.Write([]byte("1234"))
				w.Write([]byte("5678")) // 8 bytes, limit 8
			},
			wantStatus: 200,
			wantBody:   "12345678",
			wantHeader: http.Header{},
			expectOK:   true,
			limit:      8,
		},
		{
			name: "Two Write, body collected",
			run: func(w http.ResponseWriter) {
				w.Write([]byte("ab"))
				w.Write([]byte("cd"))
			},
			wantStatus: 200,
			wantBody:   "abcd",
			wantHeader: http.Header{},
			expectOK:   true,
			limit:      1024,
		},
		{
			name: "Informational WriteHeader(103), then WriteHeader(200)",
			run: func(w http.ResponseWriter) {
				w.WriteHeader(103)
				w.WriteHeader(200)
				w.Write([]byte("f"))
			},
			wantStatus: 200,
			wantBody:   "f",
			wantHeader: http.Header{},
			expectOK:   true,
			limit:      1024,
		},
		{
			name: "handler writes nothing, only Header",
			run: func(w http.ResponseWriter) {
				w.Header().Set("X-Only", "1")
			},
			wantStatus: 200,
			wantBody:   "",
			wantHeader: http.Header{"X-Only": []string{"1"}},
			expectOK:   true,
			limit:      1024,
		},
		{
			name: "Result gives body clone",
			run: func(w http.ResponseWriter) {
				w.Write([]byte("orig"))
			},
			wantStatus: 200,
			wantBody:   "orig",
			wantHeader: http.Header{},
			expectOK:   true,
			limit:      1024,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var bottom http.ResponseWriter = httptest.NewRecorder()
			// httptest.ResponseRecorder treats 1xx as the final status, so the
			// following Write is rejected. net/http does not.
			if tc.name == "Informational WriteHeader(103), then WriteHeader(200)" {
				bottom = &finalStatusWriter{header: make(http.Header)}
			}
			rec := newRecorder(bottom, tc.limit)
			tc.run(rec)

			if rec.Unwrap() != bottom {
				t.Fatalf("Unwrap() = %v, want the downstream writer", rec.Unwrap())
			}

			resp, ok := rec.result()
			if ok != tc.expectOK {
				t.Fatalf("ok = %v, want %v", ok, tc.expectOK)
			}
			if !ok {
				return
			}
			if resp.Status != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.Status, tc.wantStatus)
			}
			if string(resp.Body) != tc.wantBody {
				t.Errorf("body = %q, want %q", resp.Body, tc.wantBody)
			}
			if !reflect.DeepEqual(resp.Header, tc.wantHeader) {
				t.Errorf("header = %#v, want %#v", resp.Header, tc.wantHeader)
			}
			if tc.name == "Result gives body clone" {
				resp.Body[0] = 'X'
				again, ok := rec.result()
				if !ok || string(again.Body) != tc.wantBody {
					t.Errorf("stored body = %q after mutating result, want %q", again.Body, tc.wantBody)
				}
			}
		})
	}
}

func TestRecorderForwardsToClient(t *testing.T) {
	under := httptest.NewRecorder()
	rec := newRecorder(under, 4) // limit is smaller than the body on purpose

	rec.Header().Set("X-A", "1")
	rec.WriteHeader(http.StatusCreated)
	rec.Write([]byte("hello world"))

	if under.Code != http.StatusCreated {
		t.Errorf("client status = %d, want %d", under.Code, http.StatusCreated)
	}
	if under.Body.String() != "hello world" {
		t.Errorf("client body = %q, want %q", under.Body.String(), "hello world")
	}
	if got := under.Header().Get("X-A"); got != "1" {
		t.Errorf("client X-A = %q, want %q", got, "1")
	}
	if _, ok := rec.result(); ok {
		t.Error("result ok = true, want false: body is over the limit and must not be cached")
	}
}

func TestRecorderUnwrapFlush(t *testing.T) {
	under := httptest.NewRecorder()
	rec := newRecorder(under, 16)

	if err := http.NewResponseController(rec).Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !under.Flushed {
		t.Error("downstream writer was not flushed")
	}
}

func TestRecorderResultHeaderIsCopy(t *testing.T) {
	rec := newRecorder(httptest.NewRecorder(), 16)
	rec.Header().Set("X-A", "1")
	rec.WriteHeader(http.StatusOK)

	first, _ := rec.result()
	first.Header.Set("X-A", "changed")
	second, _ := rec.result()
	if got := second.Header.Get("X-A"); got != "1" {
		t.Errorf("stored X-A = %q, want %q", got, "1")
	}
}

// finalStatusWriter follows net/http: 1xx except 101 are interim and do not
// block a later final status or body.
type finalStatusWriter struct {
	header      http.Header
	code        int
	wroteHeader bool
	body        bytes.Buffer
}

func (w *finalStatusWriter) Header() http.Header { return w.header }

func (w *finalStatusWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		return
	}
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.code = code
}

func (w *finalStatusWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(p)
}
