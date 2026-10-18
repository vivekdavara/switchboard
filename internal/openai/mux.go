package openai

import "net/http"

// Serve routes r through mux, answering an unknown path with a 404 and a known path with the wrong method with a
// 405 (and its Allow header) in OpenAI's error shape. ServeMux would write both as text/plain.
func Serve(mux *http.ServeMux, w http.ResponseWriter, r *http.Request) {
	if _, pattern := mux.Handler(r); pattern != "" {
		mux.ServeHTTP(w, r)
		return
	}
	// Find out which of the two ServeMux would have answered, without writing anything.
	rec := &statusOnly{header: http.Header{}}
	mux.ServeHTTP(rec, r)
	if rec.status == http.StatusMethodNotAllowed {
		w.Header()["Allow"] = rec.header["Allow"]
		WriteError(w, Errorf(http.StatusMethodNotAllowed, TypeInvalidRequest, "", "method_not_allowed",
			"%s is not allowed on %s", r.Method, r.URL.Path))
		return
	}
	WriteError(w, Errorf(http.StatusNotFound, TypeInvalidRequest, "", "unknown_url", "unknown request URL: %s %s",
		r.Method, r.URL.Path))
}

// statusOnly records the status and headers a handler writes, and drops the body.
type statusOnly struct {
	header http.Header
	status int
}

func (s *statusOnly) Header() http.Header         { return s.header }
func (s *statusOnly) Write(b []byte) (int, error) { return len(b), nil }
func (s *statusOnly) WriteHeader(status int)      { s.status = status }
