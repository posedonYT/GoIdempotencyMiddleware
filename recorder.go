package idempotency

import (
	"bytes"
	"net/http"
)

// recorder wraps an http.ResponseWriter and captures the status, headers,
// and body written by a handler so they can be stored and replayed.
// A body larger than limit is discarded and cannot be cached.
type recorder struct {
	http.ResponseWriter

	status int
	header http.Header
	body   bytes.Buffer
	limit  int

	wroteHeader bool // True if header/status has been written; false means status not yet set
	// overflow indicates whether the response cannot be cached due to size or other limits
	overflow bool
}

func newRecorder(w http.ResponseWriter, limit int) *recorder {
	return &recorder{
		ResponseWriter: w,
		limit:          limit,
	}
}

// WriteHeader forwards 1xx informational statuses, except 101 Switching
// Protocols, without recording them. They are interim, and a final status
// may still follow. The first final status is stored; later calls are
// ignored, matching net/http, which sends only one final status line.
func (r *recorder) WriteHeader(code int) {
	if code >= 100 && code < 200 && code != 101 {
		r.ResponseWriter.WriteHeader(code)
		return
	}
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.header = r.ResponseWriter.Header().Clone()
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}

	n, err := r.ResponseWriter.Write(p)

	if !r.overflow {
		if r.body.Len()+n > r.limit {
			r.overflow = true
			r.body.Reset()
		} else {
			r.body.Write(p[:n])
		}
	}

	return n, err
}

// Unwrap returns the downstream ResponseWriter. http.NewResponseController
// uses it to reach Flush, deadlines, and the other optional methods.
func (r *recorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// result returns the captured response and whether it can be stored.
// The boolean is false when the body exceeded the size limit.
// Call it only after next.ServeHTTP returns.
func (r *recorder) result() (Response, bool) {
	if r.overflow {
		return Response{}, false
	}

	var (
		status int
		header http.Header
	)

	if !r.wroteHeader {
		status = http.StatusOK
		header = r.ResponseWriter.Header().Clone()
	} else {
		status = r.status
		header = r.header.Clone()
	}

	resp := Response{
		Status: status,
		Header: header,
		Body:   bytes.Clone(r.body.Bytes()),
	}

	return resp, true
}
