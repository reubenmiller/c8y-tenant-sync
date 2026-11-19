package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api"
	"github.com/reubenmiller/go-c8y/v2/pkg/c8y/api/authentication"
	"github.com/stretchr/testify/require"
)

// recordedRequest is a request received by the fake Cumulocity server
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Body   map[string]any
	// RawBody is the unparsed request body (e.g. multipart uploads)
	RawBody []byte
}

// fakeC8y is a minimal Cumulocity server for unit tests. Handlers are matched
// by "METHOD /path" (the query string is ignored); unmatched requests get 404.
type fakeC8y struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
	requests []recordedRequest
}

func newFakeC8y(t *testing.T) *fakeC8y {
	t.Helper()
	f := &fakeC8y{t: t, handlers: map[string]http.HandlerFunc{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeC8y) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	// Handlers can read the body again
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var body map[string]any
	_ = json.Unmarshal(raw, &body)

	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: body, RawBody: raw})
	handler, ok := f.handlers[r.Method+" "+r.URL.Path]
	f.mu.Unlock()

	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"test/notFound","message":"not found in fake"}`))
		return
	}
	handler(w, r)
}

// Handle registers a handler for "METHOD /path"
func (f *fakeC8y) Handle(route string, handler http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[route] = handler
}

// JSON registers a handler which responds with the given status and JSON value
func (f *fakeC8y) JSON(route string, status int, value any) {
	f.Handle(route, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, status, value)
	})
}

// Requests returns the received requests matching the method and path prefix
func (f *fakeC8y) Requests(method, pathPrefix string) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedRequest
	for _, r := range f.requests {
		if r.Method == method && strings.HasPrefix(r.Path, pathPrefix) {
			out = append(out, r)
		}
	}
	return out
}

// Client returns a client for the fake server (tenant t123)
func (f *fakeC8y) Client() *api.Client {
	f.t.Helper()
	client := api.NewClient(api.ClientOptions{
		BaseURL: f.server.URL,
		Auth:    authentication.AuthOptions{Tenant: "t123", Username: "user", Password: "pass"},
	})
	require.NotNil(f.t, client)
	return client
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// newFakeSyncer returns a syncer using the fake server
func newFakeSyncer(t *testing.T, f *fakeC8y) *Syncer {
	t.Helper()
	return &Syncer{
		Client:   f.Client(),
		Resolver: NewSourceResolver(t.TempDir(), t.TempDir(), false),
	}
}

// resultFor returns the single recorded result of a section
func resultFor(t *testing.T, s *Syncer, section string) ChangeResult {
	t.Helper()
	var found []ChangeResult
	for _, r := range s.Results {
		if r.Section == section {
			found = append(found, r)
		}
	}
	require.Len(t, found, 1, "results: %+v", s.Results)
	return found[0]
}
