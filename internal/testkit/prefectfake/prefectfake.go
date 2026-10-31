// Package prefectfake fakes the Prefect REST API calls that read a variable and a Secret block by name.
package prefectfake

import (
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/sarpaydemir/dost/internal/testkit"
)

// Fake serves the variables and Secret blocks a test sets; other names get Prefect's 404. A Secret block's value
// is obfuscated unless the request asks for include_secrets=true.
type Fake struct {
	*testkit.Server

	mu        sync.Mutex
	variables map[string]any
	secrets   map[string]any
}

// New starts a fake with no variables or Secret blocks.
func New(t testing.TB) *Fake {
	f := &Fake{Server: testkit.NewServer(t), variables: map[string]any{}, secrets: map[string]any{}}
	f.On(http.MethodGet, "/api/variables/name/{name}").DefaultFunc(func(r testkit.Request) testkit.Response {
		name, value, ok := f.lookup(f.variables, r.Params["name"])
		if !ok {
			return testkit.JSON(http.StatusNotFound, map[string]string{"detail": "Variable not found."})
		}
		return testkit.JSON(http.StatusOK, map[string]any{"name": name, "value": value, "tags": []string{}})
	})
	f.On(http.MethodGet, "/api/block_types/slug/secret/block_documents/name/{name}").DefaultFunc(func(r testkit.Request) testkit.Response {
		name, value, ok := f.lookup(f.secrets, r.Params["name"])
		if !ok {
			return testkit.JSON(http.StatusNotFound, map[string]string{"detail": "Block document not found."})
		}
		if r.Query().Get("include_secrets") != "true" {
			value = "**********"
		}
		return testkit.JSON(http.StatusOK, map[string]any{
			"name": name, "data": map[string]any{"value": value}, "block_type": map[string]string{"slug": "secret"},
		})
	})
	return f
}

// API is the base URL a Prefect client is configured with.
func (f *Fake) API() string { return f.URL() + "/api" }

// SetVariable stores value, encoded as JSON, as the variable name.
func (f *Fake) SetVariable(name string, value any) { f.set(f.variables, name, value) }

// SetSecret stores value, encoded as JSON, as the data value of the Secret block name.
func (f *Fake) SetSecret(name string, value any) { f.set(f.secrets, name, value) }

func (f *Fake) set(m map[string]any, name string, value any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m[name] = value
}

func (f *Fake) lookup(m map[string]any, escaped string) (string, any, bool) {
	name, err := url.PathUnescape(escaped)
	if err != nil {
		return "", nil, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := m[name]
	return name, v, ok
}
