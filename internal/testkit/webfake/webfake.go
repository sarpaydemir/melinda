// Package webfake serves web pages for page-fetch tests: HTML fixtures, redirects, large bodies, and, through
// testkit.Response.WithDelay, slow responses.
package webfake

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/sarpaydemir/melinda/internal/testkit"
)

// New starts a server that answers GET for each path in pages, every time.
func New(t testing.TB, pages map[string]testkit.Response) *testkit.Server {
	s := testkit.NewServer(t)
	for path, resp := range pages {
		s.On("GET", path).Default(resp)
	}
	return s
}

// HTML is a 200 text/html page.
func HTML(body string) testkit.Response {
	return testkit.Raw(http.StatusOK, "text/html; charset=utf-8", []byte(body))
}

// Redirect is a redirect to location with status, such as 302.
func Redirect(location string, status int) testkit.Response {
	return testkit.Response{Status: status, Header: http.Header{"Location": {location}}}
}

// Large is an HTML page of exactly size bytes.
func Large(size int) testkit.Response {
	const para = "<p>Lorem ipsum dolor sit amet.</p>\n"
	body := bytes.Repeat([]byte(para), size/len(para)+1)[:size]
	return testkit.Raw(http.StatusOK, "text/html; charset=utf-8", body)
}
