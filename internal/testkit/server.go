package testkit

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Server is an httptest server that records every request and answers it from scripted routes. It closes when
// the test ends.
type Server struct {
	t        testing.TB
	srv      *httptest.Server
	ctx      context.Context
	cancel   context.CancelFunc
	handlers sync.WaitGroup
	changed  Notifier

	mu       sync.Mutex
	guard    func(Request) (Response, bool)
	routes   []*Route
	requests []Request
}

// NewServer starts a server with no routes.
func NewServer(t testing.TB) *Server {
	s := newServer(t)
	s.srv.Start()
	return s
}

// NewPipeServer starts a server with no routes that is reached through the returned transport over in-memory
// pipes instead of TCP, so it can run in a synctest bubble.
func NewPipeServer(t testing.TB) (*Server, http.RoundTripper) {
	s := newServer(t)
	l := &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
	s.srv.Listener = l
	s.srv.Start()
	tr := &http.Transport{DialContext: l.dial}
	t.Cleanup(tr.CloseIdleConnections)
	return s, tr
}

func newServer(t testing.TB) *Server {
	s := &Server{t: t}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.close)
	return s
}

// pipeListener accepts the server ends of the pipes its dial creates.
type pipeListener struct {
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

func (l *pipeListener) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.closed:
	case <-ctx.Done():
	}
	_, _ = client.Close(), server.Close()
	return nil, cmp.Or(ctx.Err(), net.ErrClosed)
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// URL is the base URL of the server, without a trailing slash.
func (s *Server) URL() string { return s.srv.URL }

// Context is cancelled when the server closes; handlers that outlive a request, such as websockets, end with it.
func (s *Server) Context() context.Context { return s.ctx }

// Guard installs a check that runs before routing, such as authentication. When it reports true, its response
// is sent instead of routing the request.
func (s *Server) Guard(g func(Request) (Response, bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.guard = g
}

// On returns the route for method and pattern, creating it on first use. Pattern segments of the form {name}
// match one non-empty path segment; other segments match literally.
func (s *Server) On(method, pattern string) *Route {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.routes {
		if r.method == method && r.pattern == pattern {
			return r
		}
	}
	r := &Route{s: s, method: method, pattern: pattern, segments: strings.Split(pattern, "/")}
	s.routes = append(s.routes, r)
	return r
}

// Requests returns every request received so far, in arrival order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// Calls returns Request.Call for every request received so far.
func (s *Server) Calls() []string {
	var calls []string
	for _, r := range s.Requests() {
		calls = append(calls, r.Call())
	}
	return calls
}

// AssertCalls fails t unless the requests received so far are exactly want, in order.
func (s *Server) AssertCalls(t testing.TB, want ...string) {
	t.Helper()
	if got := s.Calls(); !slices.Equal(got, want) {
		t.Errorf("requests:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// Await blocks until at least n requests have arrived and returns them all.
func (s *Server) Await(t testing.TB, n int) []Request {
	t.Helper()
	s.changed.Await(t, fmt.Sprintf("%d requests", n), func() bool { return len(s.Requests()) >= n })
	return s.Requests()
}

func (s *Server) close() {
	s.cancel()
	s.srv.Close()
	s.handlers.Wait() // hijacked connections are not tracked by httptest
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.handlers.Add(1)
	defer s.handlers.Done()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return // the client gave up while sending
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	req := Request{Method: r.Method, Path: r.URL.EscapedPath(), RawQuery: r.URL.RawQuery, Header: r.Header.Clone(), Body: body}

	s.mu.Lock()
	guard := s.guard
	s.mu.Unlock()
	var answer func(Request) Response
	if guard != nil {
		if resp, ok := guard(req); ok {
			answer = func(Request) Response { return resp }
		}
	}
	answer, req = s.take(req, answer)
	if answer == nil {
		s.t.Errorf("testkit: unscripted request:\n%s", req)
		answer = func(Request) Response {
			return Raw(http.StatusNotImplemented, "text/plain", []byte("testkit: unscripted request\n"))
		}
	}
	s.write(w, r, answer(req))
}

// take routes req unless the guard already answered it, and records it in the same critical section, so the
// order of Requests is the order in which queued responses were handed out.
func (s *Server) take(req Request, answer func(Request) Response) (func(Request) Response, Request) {
	s.mu.Lock()
	if answer == nil {
		answer, req.Params = s.route(req)
	}
	s.requests = append(s.requests, req)
	s.mu.Unlock()
	s.changed.Notify()
	return answer, req
}

// route picks the most specific matching route that still has an answer: a queued response, which it
// dequeues, or a default. Routes with more literal segments win; among equals, the latest registered.
// s.mu must be held.
func (s *Server) route(req Request) (func(Request) Response, map[string]string) {
	path := strings.Split(req.Path, "/")
	var best *Route
	var bestParams map[string]string
	for _, r := range s.routes {
		params, ok := r.match(req.Method, path)
		if !ok || (len(r.queue) == 0 && r.answer == nil) {
			continue
		}
		if best == nil || r.literals() >= best.literals() {
			best, bestParams = r, params
		}
	}
	switch {
	case best == nil:
		return nil, nil
	case len(best.queue) > 0:
		resp := best.queue[0]
		best.queue = best.queue[1:]
		return func(Request) Response { return resp }, bestParams
	default:
		return best.answer, bestParams
	}
}

// Route answers requests that match its method and pattern: first from its queue, then from its default.
// A route with neither leaves the request to less specific routes.
type Route struct {
	s        *Server
	method   string
	pattern  string
	segments []string

	queue  []Response             // guarded by s.mu
	answer func(Request) Response // guarded by s.mu
}

// Reply queues responses, each used for one request in order.
func (r *Route) Reply(resps ...Response) *Route {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.queue = append(r.queue, resps...)
	return r
}

// Default answers every request with resp once the queue is empty.
func (r *Route) Default(resp Response) *Route {
	return r.DefaultFunc(func(Request) Response { return resp })
}

// DefaultFunc answers requests with f once the queue is empty.
func (r *Route) DefaultFunc(f func(Request) Response) *Route {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.answer = f
	return r
}

func (r *Route) match(method string, path []string) (map[string]string, bool) {
	if method != r.method || len(path) != len(r.segments) {
		return nil, false
	}
	params := map[string]string{}
	for i, seg := range r.segments {
		if name, ok := paramName(seg); ok && path[i] != "" {
			params[name] = path[i]
		} else if seg != path[i] {
			return nil, false
		}
	}
	return params, true
}

func (r *Route) literals() int {
	n := 0
	for _, seg := range r.segments {
		if _, ok := paramName(seg); !ok {
			n++
		}
	}
	return n
}

func paramName(segment string) (string, bool) {
	if len(segment) > 2 && segment[0] == '{' && segment[len(segment)-1] == '}' {
		return segment[1 : len(segment)-1], true
	}
	return "", false
}
