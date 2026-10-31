package planefake_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/planefake"
)

func TestRoutesUnderWorkspaceRequireAPIKey(t *testing.T) {
	f := planefake.New(t, "acme", "key")
	f.Route("PATCH", "/projects/{project}/work-items/{item}/").Reply(testkit.JSON(200, map[string]string{"id": "w1"}))

	send := func(key string) int {
		req, _ := http.NewRequest("PATCH", f.URL()+"/api/v1/workspaces/acme/projects/p1/work-items/w1/", strings.NewReader(`{"name":"Yeni"}`))
		req.Header.Set("X-API-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := send("wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong key: status %d, want 401", code)
	}
	if code := send("key"); code != 200 {
		t.Errorf("right key: status %d, want 200", code)
	}
	reqs := f.Requests()
	if string(reqs[1].Body) != `{"name":"Yeni"}` {
		t.Errorf("body = %s", reqs[1].Body)
	}
	if p := reqs[1].Params; p["project"] != "p1" || p["item"] != "w1" {
		t.Errorf("params = %v", p)
	}
}
