// Package planefake fakes the Plane REST API of one workspace.
package planefake

import (
	"net/http"
	"testing"

	"github.com/sarpaydemir/melinda/internal/testkit"
)

// Fake serves routes under /api/v1/workspaces/{workspace}; answers are scripted. Requests without the
// workspace's X-API-Key get 401.
type Fake struct {
	*testkit.Server
	prefix string
}

// New starts a fake for workspace that accepts apiKey.
func New(t testing.TB, workspace, apiKey string) *Fake {
	f := &Fake{Server: testkit.NewServer(t), prefix: "/api/v1/workspaces/" + workspace}
	f.Guard(func(r testkit.Request) (testkit.Response, bool) {
		return testkit.JSON(http.StatusUnauthorized, map[string]string{"detail": "Given API token is not valid"}),
			r.Header.Get("X-API-Key") != apiKey
	})
	return f
}

// Route returns the route for method and a pattern relative to the workspace, such as
// "/projects/{project}/work-items/".
func (f *Fake) Route(method, pattern string) *testkit.Route { return f.On(method, f.prefix+pattern) }
