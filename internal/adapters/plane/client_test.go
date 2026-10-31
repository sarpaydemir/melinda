package plane_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/adapters/plane"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/planefake"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

const (
	apiKey    = "k3y"
	workspace = "acme"
	project   = "p1"
	item      = "w1"
	cycle     = "c1"
	root      = "/api/v1/workspaces/acme/projects/p1"
)

// routes are the request targets of the client, each answered with {"id":"x"} by default.
var routes = [][2]string{
	{"GET", "/projects/"},
	{"GET", "/projects/{project}/labels/"},
	{"GET", "/projects/{project}/cycles/"},
	{"GET", "/projects/{project}/states/"},
	{"GET", "/projects/{project}/project-members/"},
	{"GET", "/projects/{project}/work-items/"},
	{"POST", "/projects/{project}/work-items/"},
	{"GET", "/projects/{project}/work-items/{item}/"},
	{"PATCH", "/projects/{project}/work-items/{item}/"},
	{"GET", "/projects/{project}/work-items/{item}/comments/"},
	{"POST", "/projects/{project}/work-items/{item}/comments/"},
	{"GET", "/projects/{project}/cycles/{cycle}/cycle-issues/"},
	{"POST", "/projects/{project}/cycles/{cycle}/cycle-issues/"},
}

func newFake(t *testing.T) *planefake.Fake {
	f := planefake.New(t, workspace, apiKey)
	for _, r := range routes {
		f.Route(r[0], r[1]).Default(testkit.JSON(http.StatusOK, map[string]string{"id": "x"}))
	}
	return f
}

// newClient returns a client for f over its own transport, with the endpoint given with trailing slashes.
func newClient(t *testing.T, f *planefake.Fake) *plane.Client {
	return plane.NewClient(f.URL()+"//", apiKey, workspace, transport(t), slog.New(slog.DiscardHandler))
}

func transport(t *testing.T) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

func answer(body string) testkit.Response {
	return testkit.Raw(http.StatusOK, "application/json", []byte(body))
}

// show prints a listing as the items' literals, the total, and the next-page flag.
func show(p plane.Page) string {
	return fmt.Sprintf("%s total=%s next=%s", textfmt.Repr(p.Results), textfmt.Repr(p.TotalResults), textfmt.Repr(p.NextPageResults))
}

// ids are the "id" members of a listing's items.
func ids(p plane.Page) string {
	var out []string
	for _, item := range p.Results {
		out = append(out, textfmt.Str(item.(textfmt.Object).Get("id")))
	}
	return strings.Join(out, " ")
}

var full = plane.Fields{
	Name: "N", Description: "d", StateID: "s", AssigneeIDs: []string{"a1", "a2"}, LabelIDs: []string{},
	CycleID: "cy", ParentID: "pa", Priority: "high", StartDate: "2026-01-02", TargetDate: "2026-02-03",
}

func TestRequestsOnTheWire(t *testing.T) {
	tests := []struct {
		name  string
		do    func(context.Context, *plane.Client) error
		calls []string // sorted
		body  string
	}{
		{"projects", func(ctx context.Context, c *plane.Client) error {
			_, err := c.Projects(ctx)
			return err
		}, []string{"GET /api/v1/workspaces/acme/projects/"}, ""},
		{"metadata", func(ctx context.Context, c *plane.Client) error {
			_, err := c.Metadata(ctx, project)
			return err
		}, []string{
			"GET " + root + "/cycles/?cycle_view=all", "GET " + root + "/labels/", "GET " + root + "/project-members/",
			"GET " + root + "/states/",
		}, ""},
		{"work items in order", func(ctx context.Context, c *plane.Client) error {
			_, err := c.WorkItems(ctx, project, plane.WorkItemQuery{OrderBy: "-created_at"}, 20)
			return err
		}, []string{"GET " + root + "/work-items/?per_page=100&expand=assignees&order_by=-created_at"}, ""},
		{"work items", func(ctx context.Context, c *plane.Client) error {
			_, err := c.WorkItems(ctx, project, plane.WorkItemQuery{}, 20)
			return err
		}, []string{"GET " + root + "/work-items/?per_page=100&expand=assignees"}, ""},
		{"work item", func(ctx context.Context, c *plane.Client) error {
			_, err := c.WorkItem(ctx, project, item)
			return err
		}, []string{"GET " + root + "/work-items/w1/"}, ""},
		{"create work item", func(ctx context.Context, c *plane.Client) error {
			_, err := c.CreateWorkItem(ctx, project, full)
			return err
		}, []string{"POST " + root + "/work-items/"}, `{"state":"s","assignees":["a1","a2"],"labels":[],` +
			`"description_html":"<p>d</p>","cycle_id":"cy","parent":"pa","priority":"high","start_date":"2026-01-02",` +
			`"target_date":"2026-02-03","name":"N"}`},
		{"update work item", func(ctx context.Context, c *plane.Client) error {
			_, err := c.UpdateWorkItem(ctx, project, item, full)
			return err
		}, []string{"PATCH " + root + "/work-items/w1/"}, `{"state":"s","assignees":["a1","a2"],"labels":[],` +
			`"description_html":"<p>d</p>","cycle_id":"cy","parent":"pa","name":"N","priority":"high",` +
			`"start_date":"2026-01-02","target_date":"2026-02-03"}`},
		{"comments", func(ctx context.Context, c *plane.Client) error {
			_, err := c.Comments(ctx, project, item, 20)
			return err
		}, []string{"GET " + root + "/work-items/w1/comments/?per_page=20&expand=assignees"}, ""},
		{"comments past the page size", func(ctx context.Context, c *plane.Client) error {
			_, err := c.Comments(ctx, project, item, 70)
			return err
		}, []string{"GET " + root + "/work-items/w1/comments/?per_page=50&expand=assignees"}, ""},
		{"create comment", func(ctx context.Context, c *plane.Client) error {
			_, err := c.CreateComment(ctx, project, item, "a\n\n<b> & \"q\" 'x' İş\u2028y\r\nz")
			return err
		}, []string{"POST " + root + "/work-items/w1/comments/"},
			`{"comment_html":"<p>a</p><p></p><p>&lt;b&gt; &amp; &quot;q&quot; &#x27;x&#x27; İş</p><p>y</p><p>z</p>"}`},
		{"cycle work items", func(ctx context.Context, c *plane.Client) error {
			_, err := c.CycleWorkItems(ctx, project, cycle)
			return err
		}, []string{"GET " + root + "/cycles/c1/cycle-issues/?per_page=100"}, ""},
		{"add to cycle", func(ctx context.Context, c *plane.Client) error {
			return c.AddToCycle(ctx, project, cycle, item)
		}, []string{"POST " + root + "/cycles/c1/cycle-issues/"}, `{"issues":["w1"]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			if err := tt.do(t.Context(), newClient(t, f)); err != nil {
				t.Fatal(err)
			}
			reqs := f.Requests()
			calls := f.Calls()
			slices.Sort(calls)
			if !slices.Equal(calls, tt.calls) {
				t.Errorf("requests:\n  %s\nwant:\n  %s", strings.Join(calls, "\n  "), strings.Join(tt.calls, "\n  "))
			}
			for _, r := range reqs {
				if r.Header.Get("X-API-Key") != apiKey || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("%s: headers %v", r.Call(), r.Header)
				}
				if string(r.Body) != tt.body {
					t.Errorf("%s: body %s, want %s", r.Call(), r.Body, tt.body)
				}
			}
		})
	}
}

func TestWorkItemBodiesSendOnlyTheGivenFields(t *testing.T) {
	tests := []struct {
		name   string
		fields plane.Fields
		want   string
	}{
		{"dates", plane.Fields{StartDate: "2026-06-24", TargetDate: "2026-06-24"}, `{"start_date":"2026-06-24","target_date":"2026-06-24"}`},
		{"description", plane.Fields{Description: "desc"}, `{"description_html":"<p>desc</p>"}`},
		{"blank description", plane.Fields{Description: "   "}, `{"description_html":"   "}`},
		{"name", plane.Fields{Name: "New name"}, `{"name":"New name"}`},
		{"state", plane.Fields{StateID: "uuid-1"}, `{"state":"uuid-1"}`},
		{"priority", plane.Fields{Priority: "high"}, `{"priority":"high"}`},
		{"assignees", plane.Fields{AssigneeIDs: []string{"usr-1"}}, `{"assignees":["usr-1"]}`},
		{"no assignees", plane.Fields{AssigneeIDs: []string{}}, `{"assignees":[]}`},
		{"labels", plane.Fields{LabelIDs: []string{"lbl-1"}}, `{"labels":["lbl-1"]}`},
		{"no labels", plane.Fields{LabelIDs: []string{}}, `{"labels":[]}`},
		{"nothing", plane.Fields{}, `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			c := newClient(t, f)
			_, err := c.CreateWorkItem(t.Context(), project, tt.fields)
			_, err2 := c.UpdateWorkItem(t.Context(), project, item, tt.fields)
			if err := errors.Join(err, err2); err != nil {
				t.Fatal(err)
			}
			for _, r := range f.Requests() {
				if string(r.Body) != tt.want {
					t.Errorf("%s: body %s, want %s", r.Method, r.Body, tt.want)
				}
			}
		})
	}
}

func TestListingsNormalizeTheAnswer(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"array", `[{"id":"a"},{"id":"b"}]`, "[{'id': 'a'}, {'id': 'b'}] total=None next=None"},
		{"object", `{"results":[{"id":"a"}],"total_results":7,"total_count":9,"count":3,"next_page_results":true,"next_cursor":"100:1:0"}`,
			"[{'id': 'a'}] total=7 next=True"},
		{"data and a falsy total", `{"data":[{"id":"a"}],"total_results":0,"total_count":9}`, "[{'id': 'a'}] total=9 next=None"},
		{"results before data", `{"results":[],"data":[{"id":"a"}]}`, "[] total=None next=None"},
		{"both totals falsy", `{"results":[],"total_results":0,"total_count":0}`, "[] total=0 next=None"},
		{"text", `"x"`, "[] total=None next=None"},
		{"number", `5`, "[] total=None next=None"},
		{"null", `null`, "[] total=None next=None"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			f.Route("GET", "/projects/").Reply(answer(tt.body))
			p, err := newClient(t, f).Projects(t.Context())
			if got := show(p); got != tt.want || err != nil {
				t.Errorf("got %s, %v; want %s", got, err, tt.want)
			}
		})
	}
}

func TestListingWithNullResultsIsAnError(t *testing.T) {
	f := newFake(t)
	f.Route("GET", "/projects/").Reply(answer(`{"results":null,"count":1}`))
	if p, err := newClient(t, f).Projects(t.Context()); err == nil {
		t.Errorf("got %s, want an error", show(p))
	}
}

func TestMetadataToleratesMissingLabelsAndCycles(t *testing.T) {
	ok := func(id string) testkit.Response { return answer(`{"results":[{"id":"` + id + `"}]}`) }
	fail := testkit.Status(http.StatusInternalServerError)
	tests := []struct {
		name                            string
		labels, cycles, states, members []testkit.Response
		want                            string // the metadata, or the start of the error, with {root} for the project's URL
		retried                         bool
	}{
		{"all answered", []testkit.Response{ok("l")}, []testkit.Response{ok("c")}, []testkit.Response{ok("s")}, []testkit.Response{ok("m")},
			"states=[{'id': 's'}] labels=[{'id': 'l'}] cycles=[{'id': 'c'}] members=[{'id': 'm'}]", false},
		{"labels failed", []testkit.Response{fail}, []testkit.Response{ok("c")}, []testkit.Response{ok("s")}, []testkit.Response{ok("m")},
			"states=[{'id': 's'}] labels=[] cycles=[{'id': 'c'}] members=[{'id': 'm'}]", false},
		{"cycles failed once", []testkit.Response{ok("l")}, []testkit.Response{fail, answer(`[{"id":"c2"}]`)}, []testkit.Response{ok("s")},
			[]testkit.Response{ok("m")}, "states=[{'id': 's'}] labels=[{'id': 'l'}] cycles=[{'id': 'c2'}] members=[{'id': 'm'}]", true},
		{"cycles failed twice", []testkit.Response{ok("l")}, []testkit.Response{fail, fail}, []testkit.Response{ok("s")}, []testkit.Response{ok("m")},
			"states=[{'id': 's'}] labels=[{'id': 'l'}] cycles=[] members=[{'id': 'm'}]", true},
		{"states failed", []testkit.Response{ok("l")}, []testkit.Response{ok("c")}, []testkit.Response{testkit.Status(http.StatusForbidden)},
			[]testkit.Response{ok("m")}, "Client error '403 Forbidden' for url '{root}/states/'", false},
		{"members failed", []testkit.Response{ok("l")}, []testkit.Response{ok("c")}, []testkit.Response{ok("s")}, []testkit.Response{fail},
			"Server error '500 Internal Server Error' for url '{root}/project-members/'", false},
		{"states and members failed", []testkit.Response{ok("l")}, []testkit.Response{ok("c")},
			[]testkit.Response{testkit.Status(http.StatusBadRequest)}, []testkit.Response{fail},
			"Client error '400 Bad Request' for url '{root}/states/'", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := planefake.New(t, workspace, apiKey)
			f.Route("GET", "/projects/{project}/labels/").Reply(tt.labels...)
			f.Route("GET", "/projects/{project}/cycles/").Reply(tt.cycles...)
			f.Route("GET", "/projects/{project}/states/").Reply(tt.states...)
			f.Route("GET", "/projects/{project}/project-members/").Reply(tt.members...)
			m, err := newClient(t, f).Metadata(t.Context(), project)
			got := fmt.Sprintf("states=%s labels=%s cycles=%s members=%s",
				textfmt.Repr(m.States), textfmt.Repr(m.Labels), textfmt.Repr(m.Cycles), textfmt.Repr(m.Members))
			if err != nil {
				got = err.Error()
			}
			if want := strings.ReplaceAll(tt.want, "{root}", f.URL()+root); !strings.HasPrefix(got, want) {
				t.Errorf("got %s, want %s", got, want)
			}
			if retried := slices.Contains(f.Calls(), "GET "+root+"/cycles/"); retried != tt.retried {
				t.Errorf("cycles retried without cycle_view: %v, want %v", retried, tt.retried)
			}
		})
	}
}

// items exercises every field the work-item filters read.
const items = `[
	{"id": "i1", "name": "Login bug", "identifier": "WEB-12", "sequence_id": 12, "state": "s1", "cycle_id": "c1", "assignees": ["u1"], "labels": ["l1"]},
	{"id": "i2", "name": "Ödeme", "sequence_id": 7, "state": "s9", "state_detail": {"id": "s1"}, "cycle": {"id": "c1"}, "assignee_details": [{"id": "u1"}], "label_details": [{"id": "l1"}]},
	{"id": "i3", "name": "İş planı", "sequence_id": 120, "state_details": {"id": "s2"}, "cycle_detail": "c1", "assignees": [{"id": "u2"}, "u1"], "labels": "l1"},
	{"id": "i4", "name": "y", "identifier": true, "sequence_id": 5.0, "state": {"id": "s1"}, "cycle_details": {"id": "c2"}, "assignees": "u1", "labels": [{"id": "l2"}]},
	{"id": "i5", "name": "z", "sequence_id": "٣", "state": ["s1"], "cycle_id": ["c1"], "assignees": [], "labels": [{"name": "l1"}]},
	{"id": "i6", "name": null, "state": null, "assignees": null}
]`

func TestWorkItemsAreFilteredOnTheFirstPage(t *testing.T) {
	tests := []struct {
		name  string
		query plane.WorkItemQuery
		limit int
		want  string
	}{
		{"no filter", plane.WorkItemQuery{}, 50, "i1 i2 i3 i4 i5 i6"},
		{"no filter, clipped", plane.WorkItemQuery{}, 2, "i1 i2"},
		{"state", plane.WorkItemQuery{StateID: "s1"}, 50, "i1 i2 i4 i5"},
		{"cycle", plane.WorkItemQuery{CycleID: "c1"}, 50, "i1 i2 i3 i5"},
		{"assignee", plane.WorkItemQuery{AssigneeID: "u1"}, 50, "i1 i2 i3 i4"},
		{"label", plane.WorkItemQuery{LabelID: "l1"}, 50, "i1 i2 i3"},
		{"state and label", plane.WorkItemQuery{StateID: "s1", LabelID: "l1"}, 50, "i1 i2"},
		{"name in another case", plane.WorkItemQuery{Query: "LOGIN"}, 50, "i1"},
		{"id", plane.WorkItemQuery{Query: "i2"}, 50, "i2"},
		{"identifier", plane.WorkItemQuery{Query: "web-1"}, 50, "i1"},
		{"sequence id", plane.WorkItemQuery{Query: "12"}, 50, "i1 i3"},
		{"key suffix", plane.WorkItemQuery{Query: "ABC-7"}, 50, "i2"},
		{"key suffix in other digits", plane.WorkItemQuery{Query: "x-٣"}, 50, "i5"},
		{"dotted capital I", plane.WorkItemQuery{Query: "İŞ"}, 50, "i3"},
		{"dotless query", plane.WorkItemQuery{Query: "iş"}, 50, ""},
		{"boolean identifier", plane.WorkItemQuery{Query: "True"}, 50, "i4"},
		{"float sequence id", plane.WorkItemQuery{Query: "5.0"}, 50, "i4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			f.Route("GET", "/projects/{project}/work-items/").Reply(answer(
				`{"results":` + items + `,"total_results":40,"next_page_results":true,"next_cursor":"100:1:0","count":9}`))
			p, err := newClient(t, f).WorkItems(t.Context(), project, tt.query, tt.limit)
			if err != nil {
				t.Fatal(err)
			}
			if got := ids(p); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			if p.TotalResults != int64(40) || p.NextPageResults != true {
				t.Errorf("total %s, next %s; want the server's 40 and True", textfmt.Repr(p.TotalResults), textfmt.Repr(p.NextPageResults))
			}
			f.AssertCalls(t, "GET "+root+"/work-items/?per_page=100&expand=assignees")
		})
	}
}

func TestFiltersReadTheFirst300WorkItems(t *testing.T) {
	var rows []string
	for i := range 305 {
		rows = append(rows, fmt.Sprintf(`{"id":"m%d","name":"n"}`, i))
	}
	f := newFake(t)
	f.Route("GET", "/projects/{project}/work-items/").Reply(answer("[" + strings.Join(rows, ",") + "]"))
	p, err := newClient(t, f).WorkItems(t.Context(), project, plane.WorkItemQuery{Query: "m30"}, 50)
	if got := ids(p); got != "m30" || err != nil {
		t.Errorf("got %q, %v; want m30", got, err)
	}
}

func TestCommentsAreClippedToTheLimit(t *testing.T) {
	f := newFake(t)
	f.Route("GET", "/projects/{project}/work-items/{item}/comments/").Reply(answer(
		`{"results":[{"id":"a"},{"id":"b"},{"id":"c"}],"count":3,"total_results":3}`))
	p, err := newClient(t, f).Comments(t.Context(), project, item, 2)
	if got := show(p); got != "[{'id': 'a'}, {'id': 'b'}] total=3 next=None" || err != nil {
		t.Errorf("got %s, %v", got, err)
	}
}

func TestErrorStatusesGiveTheirText(t *testing.T) {
	tests := []struct {
		name string
		resp testkit.Response
		do   func(context.Context, *plane.Client) error
		want string
	}{
		{"client error with a query", testkit.Raw(http.StatusNotFound, "application/json", []byte(`{"detail":"no"}`)),
			func(ctx context.Context, c *plane.Client) error {
				_, err := c.WorkItems(ctx, project, plane.WorkItemQuery{OrderBy: "-created_at"}, 20)
				return err
			},
			"Client error '404 Not Found' for url '%s/work-items/?per_page=100&expand=assignees&order_by=-created_at'\n" +
				"For more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/404"},
		{"server error", testkit.Raw(http.StatusInternalServerError, "text/plain", []byte("boom")),
			func(ctx context.Context, c *plane.Client) error {
				_, err := c.WorkItem(ctx, project, item)
				return err
			},
			"Server error '500 Internal Server Error' for url '%s/work-items/w1/'\n" +
				"For more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/500"},
		{"too many requests", testkit.Status(http.StatusTooManyRequests),
			func(ctx context.Context, c *plane.Client) error {
				_, err := c.CreateComment(ctx, project, item, "hi")
				return err
			},
			"Client error '429 Too Many Requests' for url '%s/work-items/w1/comments/'\n" +
				"For more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/429"},
		{"redirect", testkit.Response{Status: http.StatusFound, Header: http.Header{"Location": {"https://elsewhere/x"}}},
			func(ctx context.Context, c *plane.Client) error {
				_, err := c.WorkItem(ctx, project, item)
				return err
			},
			"Redirect response '302 Found' for url '%s/work-items/w1/'\nRedirect location: 'https://elsewhere/x'\n" +
				"For more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/302"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := planefake.New(t, workspace, apiKey)
			for _, r := range routes {
				f.Route(r[0], r[1]).Reply(tt.resp)
			}
			err := tt.do(t.Context(), newClient(t, f))
			if want := fmt.Sprintf(tt.want, f.URL()+root); err == nil || err.Error() != want {
				t.Errorf("error %v\nwant %s", err, want)
			}
			if n := len(f.Requests()); n != 1 {
				t.Errorf("%d requests, want 1", n)
			}
		})
	}
}

func TestAnswersMustBeJSON(t *testing.T) {
	for _, body := range []string{"", "<html>"} {
		f := newFake(t)
		f.Route("POST", "/projects/{project}/cycles/{cycle}/cycle-issues/").Reply(testkit.Raw(http.StatusCreated, "text/html", []byte(body)))
		if err := newClient(t, f).AddToCycle(t.Context(), project, cycle, item); err == nil {
			t.Errorf("answer %q: no error", body)
		}
	}
	f := newFake(t)
	f.Route("GET", "/projects/{project}/work-items/{item}/").Reply(testkit.Raw(http.StatusCreated, "application/json", []byte(`[1, 2]`)))
	if got, err := newClient(t, f).WorkItem(t.Context(), project, item); textfmt.Repr(got) != "[1, 2]" || err != nil {
		t.Errorf("got %s, %v; want [1, 2]", textfmt.Repr(got), err)
	}
}

func TestAnswersAreReadUpTo10MiB(t *testing.T) {
	const limit = 10 << 20
	for _, tt := range []struct {
		size int
		ok   bool
	}{{limit, true}, {limit + 1, false}} {
		f := newFake(t)
		body := append([]byte(`{"id":"w1"}`), bytes.Repeat([]byte(" "), tt.size-len(`{"id":"w1"}`))...)
		f.Route("GET", "/projects/{project}/work-items/{item}/").Reply(answer(string(body)))
		got, err := newClient(t, f).WorkItem(t.Context(), project, item)
		if (err == nil) != tt.ok {
			t.Errorf("%d bytes: got %s, %v; want ok=%v", tt.size, textfmt.Repr(got), err, tt.ok)
		}
	}
}

func TestStatusErrorsMaskTheEndpointPassword(t *testing.T) {
	f := planefake.New(t, workspace, apiKey)
	f.Route("GET", "/projects/{project}/work-items/{item}/").Reply(testkit.Status(http.StatusNotFound))
	endpoint := strings.Replace(f.URL(), "http://", "http://bot:s3cret@", 1)
	_, err := plane.NewClient(endpoint, apiKey, workspace, transport(t), slog.New(slog.DiscardHandler)).WorkItem(t.Context(), project, item)
	want := "Client error '404 Not Found' for url '" + strings.Replace(f.URL(), "http://", "http://bot:xxxxx@", 1) + root + "/work-items/w1/'\n"
	if err == nil || !strings.HasPrefix(err.Error(), want) || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error %v\nwant it to start with %s", err, want)
	}
}

func TestPathParametersAreEscaped(t *testing.T) {
	f := planefake.New(t, "my%20ws", apiKey)
	f.Route("GET", "/projects/{project}/work-items/{item}/").Default(answer(`{}`))
	c := plane.NewClient(f.URL(), apiKey, "my ws", transport(t), slog.New(slog.DiscardHandler))
	for _, pair := range [][2]string{
		{"{11111111-1111-1111-1111-111111111111}", "urn:uuid:22222222-2222-2222-2222-222222222222"},
		{"a/b", "é?x"},
	} {
		if _, err := c.WorkItem(t.Context(), pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	f.AssertCalls(t,
		"GET /api/v1/workspaces/my%20ws/projects/%7B11111111-1111-1111-1111-111111111111%7D/work-items/urn:uuid:22222222-2222-2222-2222-222222222222/",
		"GET /api/v1/workspaces/my%20ws/projects/a%2Fb/work-items/%C3%A9%3Fx/",
	)
}

func TestPathParametersThatWouldChangeThePathAreRejected(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f)
	for _, id := range []string{"", ".", ".."} {
		if _, err := c.WorkItem(t.Context(), project, id); err == nil {
			t.Errorf("work item %q: no error", id)
		}
	}
	if err := plane.NewClient(f.URL(), apiKey, "..", transport(t), slog.New(slog.DiscardHandler)).AddToCycle(t.Context(), project, cycle, item); err == nil {
		t.Error("workspace ..: no error")
	}
	f.AssertCalls(t)
}

// deadlines is a transport that records how far away each request's deadline is.
type deadlines struct {
	base http.RoundTripper
	mu   sync.Mutex
	left map[string]time.Duration
}

func (d *deadlines) RoundTrip(r *http.Request) (*http.Response, error) {
	var left time.Duration
	if dl, ok := r.Context().Deadline(); ok {
		left = time.Until(dl)
	}
	d.mu.Lock()
	d.left[r.Method+" "+r.URL.Path] = left
	d.mu.Unlock()
	return d.base.RoundTrip(r)
}

func TestEveryCallUsesTheSharedTransportWithA30SecondDeadline(t *testing.T) {
	f := newFake(t)
	rec := &deadlines{base: transport(t), left: map[string]time.Duration{}}
	c := plane.NewClient(f.URL(), apiKey, workspace, rec, slog.New(slog.DiscardHandler))
	ctx := t.Context()
	_, err := c.Projects(ctx)
	_, err2 := c.Metadata(ctx, project)
	_, err3 := c.WorkItems(ctx, project, plane.WorkItemQuery{}, 20)
	_, err4 := c.WorkItem(ctx, project, item)
	_, err5 := c.CreateWorkItem(ctx, project, plane.Fields{Name: "N"})
	_, err6 := c.UpdateWorkItem(ctx, project, item, plane.Fields{Name: "N"})
	_, err7 := c.Comments(ctx, project, item, 20)
	_, err8 := c.CreateComment(ctx, project, item, "hi")
	_, err9 := c.CycleWorkItems(ctx, project, cycle)
	err10 := c.AddToCycle(ctx, project, cycle, item)
	if err := errors.Join(err, err2, err3, err4, err5, err6, err7, err8, err9, err10); err != nil {
		t.Fatal(err)
	}
	if len(rec.left) != len(routes) || len(f.Requests()) != len(routes) {
		t.Errorf("transport saw %d calls and the server %d, want %d each", len(rec.left), len(f.Requests()), len(routes))
	}
	for call, left := range rec.left {
		if left <= 25*time.Second || left > 30*time.Second {
			t.Errorf("%s: deadline in %v, want 30s", call, left)
		}
	}
}
