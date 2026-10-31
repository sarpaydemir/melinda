package plane_test

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/adapters/plane"
	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/prompt"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/conformance"
	"github.com/sarpaydemir/dost/internal/testkit/fakeclock"
	"github.com/sarpaydemir/dost/internal/testkit/planefake"
)

// The project, work item, and cycle the tools are asked about, and work items that cycle rows name.
const (
	projectID = "8f0e4c1a-2b3d-4e5f-9a6b-7c8d9e0f1a2b"
	itemID    = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	cycleID   = "c7c1e2d3-4f5a-4b6c-8d7e-9f0a1b2c3d4e"
	uuid1     = "11111111-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	uuid2     = "22222222-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	uuid3     = "33333333-cccc-4ccc-8ccc-cccccccccccc"
)

// Request targets below the workspace.
const (
	listProjects = "GET /projects/"
	listItems    = "GET /projects/" + projectID + "/work-items/"
	listComments = "GET /projects/" + projectID + "/work-items/" + itemID + "/comments/"
	listCycle    = "GET /projects/" + projectID + "/cycles/" + cycleID + "/cycle-issues/"
	getItem      = "GET /projects/" + projectID + "/work-items/" + itemID + "/"
)

// workItemAt is the request target of the work item id.
func workItemAt(id string) string { return "GET /projects/" + projectID + "/work-items/" + id + "/" }

const (
	projects = `{"id":"` + projectID + `","name":"Web","identifier":"WEB","description":"site"},` +
		`{"id":"p2","name":null,"identifier":"","short_name":"","slug":"ops"},{"short_name":"SN"}`
	projectsListed = "Plane projects:\n[1] Web (id=" + projectID + ", identifier=WEB, workspace=acme)\n" +
		"[2] None (id=p2, identifier=ops, workspace=acme)\n[3] ? (id=?, identifier=SN, workspace=acme)"
	web       = `{"id":"` + projectID + `","name":"Web","identifier":"WEB"}`
	webListed = "Plane projects:\n[1] Web (id=" + projectID + ", identifier=WEB, workspace=acme)"
	workItems = `{"total_results":4,"results":[{"id":"` + uuid1 + `","name":"Fix login","sequence_id":12,"priority":"high",` +
		`"state":"s1","state_detail":{"id":"s1","name":"Todo"},"assignees":[{"id":"u1","display_name":"ada",` +
		`"first_name":"Ada"},{"id":"u2","first_name":"Bob"},{"id":"u3","display_name":""}]},{"id":"` + uuid2 + `",` +
		`"name":"Write docs","identifier":"WEB-13","sequence_id":13,"priority":null,"state":"s2","assignees":["u1","u2"]},` +
		`{"id":"` + uuid3 + `","name":null,"sequence_id":0,"priority":"none","state_details":{"name":""},"state":null,` +
		`"assignees":[]},{"sequence_id":7,"assignees":[5,null,"u"],"assignee_details":[]},{"id":"i5","assignees":"ab"}]}`
	fetched = `{"id":"` + uuid1 + `","name":"Fetched one","sequence_id":1,"state_detail":{"name":"Doing"},` +
		`"assignees":[{"id":"u1","display_name":"ada"}],"priority":"urgent","start_date":"2026-10-01"}`
	fetchedRow = "1 Fetched one (id=" + uuid1 + ", state=Doing, assignees=ada, priority=urgent, start=2026-10-01, target=?)"
)

// noWait is a clock on which every wait is over at once.
type noWait struct{ *fakeclock.Clock }

func (c noWait) NewTimer(time.Duration) clock.Timer { return c.Clock.NewTimer(0) }

// monthAliases name February in a second way.
const monthAliases = "\nşubat, subat"

// planeTools are Plane's tools with the settings document doc and the default texts, with monthAliases. It is now
// on their clock, on which they never wait to retry.
func planeTools(t *testing.T, doc string, now time.Time) []tool.Tool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatal(err)
	}
	s, err := config.Load(m, "agent", nil, func(*config.Settings) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := prompt.Load(plane.Prompts(), "")
	if err != nil {
		t.Fatal(err)
	}
	texts, err := defaults.With(map[string]string{"plane.month_aliases": monthAliases})
	if err != nil {
		t.Fatal(err)
	}
	return plane.Tools(s, texts, transport(t), noWait{fakeclock.New(now)}, slog.New(slog.DiscardHandler))
}

// configured is the settings document for the workspace f serves.
func configured(f *planefake.Fake) string {
	return `{"model":"m","plane_endpoint":"` + f.URL() + `","plane_api_key":"` + apiKey + `","plane_workspace_slug":"` + workspace + `"}`
}

// exchange is a call of a tool on a fake workspace and what comes of it.
type exchange struct {
	name, tool, args string
	// settings, when set, is the settings document instead of one for the fake.
	settings string
	// routes answer each "METHOD /path" below the workspace with their responses in turn, the last one from then on.
	routes map[string][]testkit.Response
	// requester names the user being answered; today is the date, 2026-10-31 when empty; refuse refuses writes.
	requester, today string
	refuse           error
	// want is the result, or the error's text when failed, with the fake's address written as {endpoint}.
	want   string
	failed bool
	// calls are the requests made, below the workspace, each followed by its body after a space when it has one.
	calls []string
}

func (x exchange) check(t *testing.T) {
	f := planefake.New(t, workspace, apiKey)
	for target, answers := range x.routes {
		method, path, _ := strings.Cut(target, " ")
		f.Route(method, path).Reply(answers[:len(answers)-1]...).Default(answers[len(answers)-1])
	}
	doc := x.settings
	if doc == "" {
		doc = configured(f)
	}
	now, err := time.Parse(time.DateOnly, cmp.Or(x.today, "2026-10-31"))
	if err != nil {
		t.Fatal(err)
	}
	lookups := 0
	got, err := call(t, doc, now, x.tool, tool.Call{
		Args:        json.RawMessage(x.args),
		Requester:   func(context.Context) string { lookups++; return x.requester },
		BeforeWrite: func(context.Context) error { return x.refuse },
	})
	if lookups > 1 {
		t.Errorf("the requester was looked up %d times", lookups)
	}
	if err != nil {
		got = err.Error()
	}
	if got = strings.ReplaceAll(got, f.URL(), "{endpoint}"); got != x.want || (err != nil) != x.failed {
		t.Errorf("%s(%s) = %q (error: %v)\nwant %q (error: %v)", x.tool, x.args, got, err != nil, x.want, x.failed)
	}
	var calls []string
	for _, r := range f.Requests() {
		c := strings.Replace(r.Call(), " /api/v1/workspaces/"+workspace, " ", 1)
		if len(r.Body) > 0 {
			c += " " + string(r.Body)
		}
		calls = append(calls, c)
	}
	slices.Sort(calls)
	if want := slices.Sorted(slices.Values(x.calls)); !slices.Equal(calls, want) {
		t.Errorf("requests %q, want %q", calls, want)
	}
}

// call makes the call c of the tool name at the time now, with the settings document doc.
func call(t *testing.T, doc string, now time.Time, name string, c tool.Call) (string, error) {
	t.Helper()
	tools := planeTools(t, doc, now)
	i := slices.IndexFunc(tools, func(tl tool.Tool) bool { return tl.Name() == name })
	if i < 0 {
		t.Fatalf("no tool %s", name)
	}
	return tools[i].Call(t.Context(), c)
}

func run(t *testing.T, exchanges []exchange) {
	for _, x := range exchanges {
		t.Run(x.name, x.check)
	}
}

// listing is the answer {"total_results": total, "results": [items]}.
func listing(total, items string) testkit.Response {
	return answer(`{"total_results":` + total + `,"results":[` + items + `]}`)
}

func TestToolsKeepTheToolContract(t *testing.T) {
	f := planefake.New(t, workspace, apiKey)
	f.Route("GET", "/projects/").Default(answer(`[]`))
	conformance.Tools(t, planeTools(t, configured(f), time.Unix(0, 0)), plane.Prompts())
}

func TestOnlyTheWriteToolsAreOutbound(t *testing.T) {
	var outbound []string
	for _, tl := range planeTools(t, `{"model":"m"}`, time.Unix(0, 0)) {
		if _, ok := tl.(tool.Outbound); ok {
			outbound = append(outbound, tl.Name())
		}
	}
	if want := []string{"plane_create_issue", "plane_update_issue", "plane_create_comment"}; !slices.Equal(outbound, want) {
		t.Errorf("outbound tools %q, want %q", outbound, want)
	}
}

func TestUnusableSettingsAreReportedAfterTheArgumentChecks(t *testing.T) {
	run(t, []exchange{
		{name: "none at all", tool: "plane_list_projects", args: `{}`, settings: `{"model":"m"}`,
			want: "Error: Missing Plane config: plane_endpoint, plane_api_secret, plane_workspace_slug"},
		{name: "an empty key", tool: "plane_list_projects", args: `{}`,
			settings: `{"model":"m","plane_endpoint":"http://plane","plane_api_key":"","plane_workspace_slug":"acme"}`,
			want:     "Error: Missing Plane config: plane_api_secret"},
		{name: "a null endpoint and no workspace", tool: "plane_list_projects", args: `{}`,
			settings: `{"model":"m","plane_endpoint":null,"plane_api_key":"k"}`,
			want:     "Error: Missing Plane config: plane_endpoint, plane_workspace_slug"},
		{name: "before the UUID check", tool: "plane_get_project_metadata", args: `{"project_id": "WEB"}`,
			settings: `{"model":"m","plane_endpoint":"http://plane"}`,
			want:     "Error: Missing Plane config: plane_api_secret, plane_workspace_slug"},
		{name: "after the argument checks", tool: "plane_get_work_item", args: `{"project_id": "` + projectID + `"}`,
			settings: `{"model":"m"}`, want: "Error: Missing work_item_id"},
		{name: "a value of the wrong type", tool: "plane_list_projects", args: `{}`,
			settings: `{"model":"m","plane_endpoint":5,"plane_api_key":"k","plane_workspace_slug":"acme"}`,
			failed:   true, want: "plane_endpoint: expected string"},
	})
}

func TestArgumentsAreCheckedBeforeAnyRequest(t *testing.T) {
	const (
		projectUUID = "Error: project_id must be a Plane UUID. Call plane_list_projects first to resolve the project name or identifier."
		itemUUID    = "Error: work_item_id must be a Plane UUID. Call plane_list_issues first to resolve the issue key/title."
		order       = "Error: order_by must be one of: completed_at, created_at, name, priority, sequence_id, sort_order, " +
			"start_date, target_date, updated_at. Prefix with '-' for descending order."
	)
	exchanges := []exchange{
		{name: "blank project", tool: "plane_get_project_metadata", args: `{"project_id": "  "}`, want: "Error: Missing project_id"},
		{name: "falsy project", tool: "plane_get_project_metadata", args: `{"project_id": 0}`, want: "Error: Missing project_id"},
		{name: "numeric project", tool: "plane_get_project_metadata", args: `{"project_id": 5}`, failed: true,
			want: "'int' object has no attribute 'strip'"},
		{name: "null arguments", tool: "plane_get_project_metadata", args: `null`, failed: true,
			want: "'NoneType' object has no attribute 'get'"},
		{name: "arguments that are not JSON", tool: "plane_get_project_metadata", args: `{`, want: "Error: Missing project_id"},
		{name: "a project key", tool: "plane_get_project_metadata", args: `{"project_id": "WEB"}`, want: projectUUID},
		{name: "31 digits", tool: "plane_get_project_metadata", args: `{"project_id": "` + projectID[:35] + `"}`, want: projectUUID},
		{name: "project before filters", tool: "plane_list_issues", args: `{"query": 5}`, want: "Error: Missing project_id"},
		{name: "filters before the UUID check", tool: "plane_list_issues", args: `{"project_id": "WEB", "assignee_id": ["x"]}`,
			failed: true, want: "'list' object has no attribute 'strip'"},
		{name: "order_by read with the filters", tool: "plane_list_issues", args: `{"project_id": "WEB", "order_by": {"a": 1}}`,
			failed: true, want: "'dict' object has no attribute 'strip'"},
		{name: "UUID check before order_by", tool: "plane_list_issues", args: `{"project_id": "WEB", "order_by": "nope"}`,
			want: projectUUID},
		{name: "comments without project", tool: "plane_list_comments", args: `{"work_item_id": "` + itemID + `"}`,
			want: "Error: Missing project_id"},
		{name: "comments without work item", tool: "plane_list_comments", args: `{"project_id": "` + projectID + `"}`,
			want: "Error: Missing work_item_id"},
		{name: "both read before either is missing", tool: "plane_list_comments", args: `{"project_id": "", "work_item_id": 5}`,
			failed: true, want: "'int' object has no attribute 'strip'"},
		{name: "comments on a project key", tool: "plane_list_comments", args: `{"project_id": "WEB", "work_item_id": "WEB-12"}`,
			want: projectUUID},
		{name: "comments on a work item key", tool: "plane_list_comments",
			args: `{"project_id": "` + projectID + `", "work_item_id": "WEB-12"}`, want: itemUUID},
		{name: "null cycle", tool: "plane_list_cycle_issues", args: `{"project_id": "` + projectID + `", "cycle_id": null}`,
			want: "Error: Missing cycle_id"},
		{name: "cycle without project", tool: "plane_list_cycle_issues", args: `{"cycle_id": "` + cycleID + `"}`,
			want: "Error: Missing project_id"},
		{name: "cycle name", tool: "plane_list_cycle_issues", args: `{"project_id": "` + projectID + `", "cycle_id": "October"}`,
			want: "Error: cycle_id must be a Plane UUID. Call plane_get_project_metadata first to resolve cycle names or months."},
		{name: "cycle of a project name", tool: "plane_list_cycle_issues", args: `{"project_id": "Web", "cycle_id": "October"}`,
			want: projectUUID},
		{name: "empty work item", tool: "plane_get_work_item", args: `{"project_id": "` + projectID + `", "work_item_id": ""}`,
			want: "Error: Missing work_item_id"},
		{name: "work item key", tool: "plane_get_work_item", args: `{"project_id": "` + projectID + `", "work_item_id": "WEB-12"}`,
			want: itemUUID},
		{name: "work item of a project name", tool: "plane_get_work_item", args: `{"project_id": "x", "work_item_id": "WEB-12"}`,
			want: projectUUID},
	}
	for _, by := range []string{"--name", "-", "title", "Name", "created"} {
		exchanges = append(exchanges, exchange{name: "order_by " + by, tool: "plane_list_issues",
			args: `{"project_id": "` + projectID + `", "order_by": "` + by + `"}`, want: order})
	}
	run(t, exchanges)
}

func TestProjectsAreListedWithTheirIdentifiers(t *testing.T) {
	calls := []string{listProjects}
	run(t, []exchange{
		{name: "listed", tool: "plane_list_projects", args: `{}`, calls: calls,
			routes: map[string][]testkit.Response{listProjects: {listing("3", projects)}}, want: projectsListed},
		{name: "whatever the arguments", tool: "plane_list_projects", args: `null`, calls: calls,
			routes: map[string][]testkit.Response{listProjects: {answer(`[` + projects + `]`)}}, want: projectsListed},
		{name: "listed under data", tool: "plane_list_projects", args: `{}`, calls: calls,
			routes: map[string][]testkit.Response{listProjects: {answer(`{"data":[` + web + `]}`)}}, want: webListed},
		{name: "none", tool: "plane_list_projects", args: `{}`, calls: calls,
			routes: map[string][]testkit.Response{listProjects: {listing("0", "")}}, want: "No Plane projects found."},
		{name: "an answer that lists nothing", tool: "plane_list_projects", args: `{}`, calls: calls,
			routes: map[string][]testkit.Response{listProjects: {answer(`"text"`)}}, want: "No Plane projects found."},
	})
}

func TestListingsNoteHowManyResultsTheyShow(t *testing.T) {
	more := "\n\nShowing 1 results. More results may exist; narrow the query if needed."
	var exchanges []exchange
	for _, c := range []struct{ name, fields, note string }{
		{"a larger total", `"total_results":120`, "\n\nShowing 1 of 120 results."},
		{"the total count when the total is 0", `"total_results":0,"total_count":9`, "\n\nShowing 1 of 9 results."},
		{"a fractional total", `"total_results":5.0`, "\n\nShowing 1 of 5.0 results."},
		{"a next page", `"total_results":null,"next_page_results":true`, more},
		{"a next page and an equal total", `"total_results":1,"next_page_results":true`, more},
	} {
		exchanges = append(exchanges, exchange{name: c.name, tool: "plane_list_projects", args: `{}`, calls: []string{listProjects},
			routes: map[string][]testkit.Response{listProjects: {answer(`{` + c.fields + `,"results":[` + web + `]}`)}},
			want:   webListed + c.note})
	}
	run(t, exchanges)
}

func TestMetadataListsEachSectionThatHasEntries(t *testing.T) {
	at := "GET /projects/" + projectID
	calls := []string{at + "/cycles/?cycle_view=all", at + "/labels/", at + "/project-members/", at + "/states/"}
	routes := func(states, labels, cycles, members string) map[string][]testkit.Response {
		return map[string][]testkit.Response{at + "/states/": {answer(states)}, at + "/labels/": {answer(labels)},
			at + "/cycles/": {answer(cycles)}, at + "/project-members/": {answer(members)}}
	}
	run(t, []exchange{
		{name: "every section", tool: "plane_get_project_metadata", args: `{"project_id": " ` + projectID + ` "}`, calls: calls,
			routes: routes(
				`[{"id":"s1","name":"Todo","color":"#fff","group":"unstarted","default":true},{"id":"s2","group":"",`+
					`"category":"done","completed":false,"is_final":true,"is_completed":true},{"id":"s3","name":null,"group":5}]`,
				`[{"id":"l1","name":"bug","color":"#f00"},{"id":"l2"}]`,
				`[{"id":"`+cycleID+`","name":"2026-10","start_date":"2026-10-01","end_date":"2026-10-31"},`+
					`{"id":"cy2","name":"Backlog","start_date":null}]`,
				`[{"id":"u1","first_name":"Ada","last_name":"L","display_name":"ada","avatar":""},{"id":"m2","member":{"id":"u2",`+
					`"display_name":"bob","username":"bobby"}},{"id":"m3","member":{"first_name":"Cem","username":"cem"}},`+
					`{"id":"m4","display_name":"","first_name":null},{"id":"m5"},{"id":"m6","member":null,"display_name":"x",`+
					`"username":""},{"id":"m7","member":{},"first_name":"y","username":"u"},{"member":{"display_name":"z",`+
					`"username":7,"id":null}}]`),
			want: "Plane metadata for project_id=" + projectID + "\n\n" +
				"States:\n- Todo (id=s1, group=unstarted)\n" +
				"- s2 (id=s2, category=done, is_completed=True, completed=False, is_final=True)\n- None (id=s3)\n\n" +
				"Labels:\n- bug (id=l1)\n- l2 (id=l2)\n\n" +
				"Cycles:\n- 2026-10 (id=" + cycleID + ", starts=2026-10-01, ends=2026-10-31)\n- Backlog (id=cy2, starts=?, ends=?)\n\n" +
				"Members:\n- ada (id=u1)\n- bob (@bobby) (id=u2)\n- Cem (@cem) (id=m3)\n- None (id=m4)\n-  (id=m5)\n- x (id=m6)\n" +
				"- y (@u) (id=m7)\n- z (@7) (id=None)"},
		{name: "members only", tool: "plane_get_project_metadata", args: `{"project_id": "` + projectID + `"}`, calls: calls,
			routes: routes(`[]`, `[]`, `[]`, `[{"id":"u1","display_name":"ada"}]`),
			want:   "Plane metadata for project_id=" + projectID + "\n\nMembers:\n- ada (id=u1)"},
		{name: "none", tool: "plane_get_project_metadata", args: `{"project_id": "` + projectID + `"}`, calls: calls,
			routes: routes(`[]`, `[]`, `[]`, `[]`), want: "Plane metadata for project_id=" + projectID},
	})
}

func TestWorkItemsShowKeyStateAssigneesAndPriority(t *testing.T) {
	newest := []string{listItems + "?per_page=100&expand=assignees&order_by=-created_at"}
	docs := "WEB-13 Write docs (id=" + uuid2 + ", state=s2, assignees=u1, u2, priority=none)"
	run(t, []exchange{
		{name: "listed", tool: "plane_list_issues", args: `{"project_id": "` + projectID + `"}`, calls: newest,
			routes: map[string][]testkit.Response{listItems: {answer(workItems)}},
			want: "Plane issues:\n[1] 12 Fix login (id=" + uuid1 + ", state=Todo, assignees=ada, Bob, u3, priority=high)\n[2] " + docs +
				"\n[3] None (id=" + uuid3 + ", state=?, assignees=none, priority=none)\n" +
				"[4] 7 ? (id=?, state=?, assignees=5, None, u, priority=none)\n[5] ? (id=i5, state=?, assignees=a, b, priority=none)"},
		{name: "none", tool: "plane_list_issues", args: `{"project_id": "` + projectID + `"}`, calls: newest,
			routes: map[string][]testkit.Response{listItems: {listing("0", "")}}, want: "No Plane issues found."},
		{name: "matching a key", tool: "plane_list_issues", args: `{"project_id": "` + projectID + `", "query": "web-13"}`,
			calls: newest, routes: map[string][]testkit.Response{listItems: {answer(workItems)}},
			want: "Plane issues:\n[1] " + docs + "\n\nShowing 1 of 4 results."},
		{name: "filtered by names, which are not checked", tool: "plane_list_issues",
			args:  `{"project_id": "` + projectID + `", "state_id": "Todo", "cycle_id": "October"}`,
			calls: newest, routes: map[string][]testkit.Response{listItems: {answer(workItems)}}, want: "No Plane issues found."},
		{name: "by name descending", tool: "plane_list_issues", args: `{"project_id": "` + projectID + `", "order_by": " -name "}`,
			calls:  []string{listItems + "?per_page=100&expand=assignees&order_by=-name"},
			routes: map[string][]testkit.Response{listItems: {answer(`[]`)}}, want: "No Plane issues found."},
		{name: "by sequence", tool: "plane_list_issues", args: `{"project_id": "` + projectID + `", "order_by": "sequence_id"}`,
			calls:  []string{listItems + "?per_page=100&expand=assignees&order_by=sequence_id"},
			routes: map[string][]testkit.Response{listItems: {answer(`[]`)}}, want: "No Plane issues found."},
		{name: "newest first by default", tool: "plane_list_issues", args: `{"project_id": "` + projectID + `", "order_by": "  "}`,
			calls: newest, routes: map[string][]testkit.Response{listItems: {answer(`[]`)}}, want: "No Plane issues found."},
	})
}

func TestWorkItemLimitIsBetween1And50(t *testing.T) {
	var items, rows []string
	for i := range 60 {
		items = append(items, fmt.Sprintf(`{"id":"i%d"}`, i))
		rows = append(rows, fmt.Sprintf("\n[%d] ? (id=i%d, state=?, assignees=none, priority=none)", i+1, i))
	}
	var exchanges []exchange
	for _, c := range []struct {
		limit string
		shown int
	}{{`2`, 2}, {`0`, 20}, {`-5`, 1}, {`" 3 "`, 3}, {`true`, 1}, {`[1]`, 20}, {`2.9`, 2}, {`99`, 50}, {`"abc"`, 20}} {
		exchanges = append(exchanges, exchange{name: c.limit, tool: "plane_list_issues",
			args:   `{"project_id": "` + projectID + `", "limit": ` + c.limit + `}`,
			routes: map[string][]testkit.Response{listItems: {listing("60", strings.Join(items, ","))}},
			want:   "Plane issues:" + strings.Join(rows[:c.shown], "") + fmt.Sprintf("\n\nShowing %d of 60 results.", c.shown),
			calls:  []string{listItems + "?per_page=100&expand=assignees&order_by=-created_at"}})
	}
	run(t, exchanges)
}

func TestCommentsShowDateAuthorAndText(t *testing.T) {
	const (
		first = `{"id":"c1","created_at":"2026-10-30T09:15:22.123456Z","actor_detail":{"id":"u1","display_name":"ada"},` +
			`"actor":"u1","comment_html":"<p>Hello &amp; welcome<br/>second&nbsp;line</p><ul><li>one</li><LI class=\"x\">two</LI>` +
			`</ul><div>tail   </div>"}`
		header = "Plane comments for work_item_id=" + itemID + "\n\n"
		hello  = "[1] 2026-10-30 09:15 | ada\nHello & welcome\nsecond\u00a0line\n* one\n* two\n\ntail\n\n"
	)
	const comments = first + `,"plain text row",{"id":"c3","created_on":"2026-10-29","comment":"&lt;b&gt;bold&lt;/b&gt; text",` +
		`"actor":"u2"},{"id":"c4","created_at":"","comment_html":"","created_by":{"first_name":"Cem"}},` +
		`{"created_at":"2026-10-30T09:15:22Z","actor_detail":"","actor":"","created_by":"u1","comment":"<b>x</b>"},` +
		`{"created_on":"2026-10-30T23:59","actor_detail":{"first_name":"F","id":"i"},"comment_html":"&amp;lt;p&amp;gt;"},` +
		`{"actor_detail":{"id":"only-id"},"comment_html":"   "},{"actor_detail":{"display_name":""},"comment_html":null,` +
		`"comment":"plain"},null,[1],{"a":1}`
	calls := func(n string) []string { return []string{listComments + "?per_page=" + n + "&expand=assignees"} }
	run(t, []exchange{
		{name: "listed", tool: "plane_list_comments", args: `{"project_id": "` + projectID + `", "work_item_id": "` + itemID + `"}`,
			calls: calls("20"), routes: map[string][]testkit.Response{listComments: {listing("11", comments)}},
			want: header + hello + "[2] plain text row\n\n[3] 2026-10-29 | u2\nbold text\n\n[4]  | Cem\n\n\n" +
				"[5] 2026-10-30 09:15 | u1\nx\n\n[6] 2026-10-30 23:59 | F\n&lt;p&gt;\n\n[7]  | only-id\n\n\n[8]  | ?\nplain\n\n" +
				"[9] None\n\n[10] [1]\n\n[11]  | ?"},
		{name: "two of more", tool: "plane_list_comments",
			args:  `{"project_id": "` + projectID + `", "work_item_id": "` + itemID + `", "limit": 2}`,
			calls: calls("2"), routes: map[string][]testkit.Response{listComments: {listing("30", comments)}},
			want: header + hello + "[2] plain text row\n\n\nShowing 2 of 30 results."},
		{name: "fifty at most, of a first page", tool: "plane_list_comments",
			args:  `{"project_id": "` + projectID + `", "work_item_id": "` + itemID + `", "limit": 70}`,
			calls: calls("50"), routes: map[string][]testkit.Response{listComments: {answer(`{"total_results":null,` +
				`"next_page_results":true,"results":[` + first + `]}`)}},
			want: header + hello + "\nShowing 1 results. More results may exist; narrow the query if needed."},
		{name: "none", tool: "plane_list_comments", args: `{"project_id": "` + projectID + `", "work_item_id": "` + itemID + `"}`,
			calls: calls("20"), routes: map[string][]testkit.Response{listComments: {answer(`[]`)}},
			want: "No Plane comments for work_item_id=" + itemID + "."},
	})
}

func TestCycleRowsShowTheirWorkItems(t *testing.T) {
	const rows = `{"id":"r1","issue":"` + uuid1 + `","cycle":"` + cycleID + `"},{"id":"r2","issue":{"id":"` + uuid2 + `",` +
		`"name":"Embedded","sequence_id":2,"start_date":"2026-10-02"},"issue_detail":{"name":"detail"}},"loose row",` +
		`{"id":"r4","issue":"WEB-4","warning":"server says hi"},{"id":"i4","name":"Plain item","sequence_id":4,` +
		`"priority":"low","state":"s1","target_date":"2026-11-01"},{"id":"r6","issue":"` + uuid3 + `"},{"id":"r7",` +
		`"issue":{},"issue_detail":{"id":"d7","name":"From detail"}},{"id":"r8","work_item":{"id":"w8","name":"Own work item"},` +
		`"issue":" ` + uuid2 + ` "},{"name":"no id"},{},null,[1,2],{"issue":null,"issue_detail":"text","warning":""},` +
		`{"issue":{"name":"embedded"},"work_item":{"name":"own"},"warning":5}`
	spaced := "GET /projects/" + projectID + "/work-items/%20" + uuid2 + "%20/"
	routes := map[string][]testkit.Response{
		listCycle:         {answer(`{"total_results":30,"next_page_results":true,"results":[` + rows + `]}`)},
		workItemAt(uuid1): {answer(fetched)}, workItemAt(uuid3): {testkit.Status(404)}, spaced: {answer(`{"id":"` + uuid2 + `"}`)},
	}
	calls := []string{listCycle + "?per_page=100", spaced, workItemAt(uuid1), workItemAt(uuid3)}
	header := "Plane cycle issues for project_id=" + projectID + ", cycle_id=" + cycleID + ":\nTotal fetched cycle items: "
	none := "state=?, assignees=none, priority=none, start=?, target=?)"
	page := listCycle + "?per_page=100"
	run(t, []exchange{
		{name: "rows that need no request first", tool: "plane_list_cycle_issues",
			args: `{"project_id": "` + projectID + `", "cycle_id": "` + cycleID + `"}`, routes: routes, calls: calls,
			want: header + "14\n\nShowing first 14 detailed rows:\n" +
				"[1] 2 Embedded (id=" + uuid2 + ", state=?, assignees=none, priority=none, start=2026-10-02, target=?)\n" +
				"[2] work_item_id=loose row\n[3] work_item_id=WEB-4\nWarning: server says hi\n" +
				"[4] 4 Plain item (id=i4, state=s1, assignees=none, priority=low, start=?, target=2026-11-01)\n" +
				"[5] From detail (id=d7, " + none + "\n[6] no id (id=None, " + none + "\n[7] ? (id=None, " + none + "\n" +
				"[8] work_item_id=None\n[9] work_item_id=[1, 2]\n[10] work_item_id=text\n" +
				"[11] embedded (id={'name': 'embedded'}, " + none + "\nWarning: 5\n[12] " + fetchedRow + "\n" +
				"[13] work_item_id=" + uuid3 + "\nWarning: Could not enrich work_item_id=" + uuid3 + ": Client error '404 Not Found' " +
				"for url '{endpoint}/api/v1/workspaces/acme/projects/" + projectID + "/work-items/" + uuid3 + "/'\n" +
				"For more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/404\n" +
				"[14] ? (id=" + uuid2 + ", " + none + "\n\nShowing 14 of 30 results."},
		{name: "the first few", tool: "plane_list_cycle_issues",
			args:   `{"project_id": "` + projectID + `", "cycle_id": "` + cycleID + `", "limit": 3}`,
			routes: routes, calls: calls,
			want: header + "14\n\nShowing first 3 detailed rows:\n" +
				"[1] 2 Embedded (id=" + uuid2 + ", state=?, assignees=none, priority=none, start=2026-10-02, target=?)\n" +
				"[2] work_item_id=loose row\n[3] work_item_id=WEB-4\nWarning: server says hi\n\nShowing 14 of 30 results."},
		{name: "twenty by default", tool: "plane_list_cycle_issues",
			args:   `{"project_id": "` + projectID + `", "cycle_id": "` + cycleID + `", "limit": 0}`,
			routes: map[string][]testkit.Response{listCycle: {answer(`[` + strings.Repeat(`{"id":"a"},`, 24) + `{"id":"a"}]`)}},
			calls:  []string{page},
			want: header + "25\n\nShowing first 20 detailed rows:" + func() (s string) {
				for i := range 20 {
					s += fmt.Sprintf("\n[%d] ? (id=a, %s", i+1, none)
				}
				return s
			}()},
		{name: "the first 500", tool: "plane_list_cycle_issues",
			args: `{"project_id": "` + projectID + `", "cycle_id": "` + cycleID + `", "limit": 1}`,
			routes: map[string][]testkit.Response{listCycle: {answer(`{"total_results":503,"results":[` + func() string {
				rows := make([]string, 503)
				for i := range rows {
					rows[i] = fmt.Sprintf(`{"id":"x%d"}`, i)
				}
				return strings.Join(rows, ",")
			}() + `]}`)}},
			calls: []string{page},
			want:  header + "500\n\nShowing first 1 detailed rows:\n[1] ? (id=x0, " + none + "\n\nShowing 500 of 503 results."},
		{name: "none", tool: "plane_list_cycle_issues", args: `{"project_id": "` + projectID + `", "cycle_id": "` + cycleID + `"}`,
			routes: map[string][]testkit.Response{listCycle: {listing("0", "")}}, calls: []string{page},
			want: "No Plane cycle issues found for project_id=" + projectID + ", cycle_id=" + cycleID + "."},
	})
}

func TestCycleWorkItemsAreFetchedForAtMost50Rows5AtATime(t *testing.T) {
	f := planefake.New(t, workspace, apiKey)
	id := func(i int) string { return fmt.Sprintf("%08d-0000-4000-8000-000000000000", i) }
	rows := make([]string, 53)
	for i := range rows {
		rows[i] = `{"id":"m` + fmt.Sprint(i) + `","issue":"` + id(i) + `"}`
	}
	f.Route("GET", strings.TrimPrefix(listCycle, "GET ")).Default(answer(`[` + strings.Join(rows, ",") + `]`))
	var mu sync.Mutex
	var running, most int
	// full opens 20 ms after 5 requests are in flight (time for a sixth to show up), or after a second without that.
	full := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(full) }) }
	f.Route("GET", "/projects/"+projectID+"/work-items/{item}/").DefaultFunc(func(testkit.Request) testkit.Response {
		mu.Lock()
		running++
		most = max(most, running)
		if running == 5 {
			time.AfterFunc(20*time.Millisecond, open)
		}
		mu.Unlock()
		select {
		case <-full:
		case <-time.After(time.Second):
			open()
		}
		mu.Lock()
		running--
		mu.Unlock()
		return answer(`{"name":"enriched"}`)
	})
	got, err := call(t, configured(f), time.Unix(0, 0), "plane_list_cycle_issues",
		tool.Call{Args: json.RawMessage(`{"project_id": "` + projectID + `", "cycle_id": "` + cycleID + `", "limit": 50}`)})
	want := "Plane cycle issues for project_id=" + projectID + ", cycle_id=" + cycleID + ":\nTotal fetched cycle items: 53\n\n" +
		"Showing first 50 detailed rows:"
	for i := 50; i < 53; i++ {
		want += fmt.Sprintf("\n[%d] work_item_id=%s\nWarning: Enrichment skipped for %s (limit 50); use plane_get_work_item if needed",
			i-49, id(i), id(i))
	}
	for i := range 47 {
		want += fmt.Sprintf("\n[%d] enriched (id=%s, state=?, assignees=none, priority=none, start=?, target=?)", i+4, id(i))
	}
	if err != nil || got != want {
		t.Errorf("got %q, %v\nwant %q", got, err, want)
	}
	if n := len(f.Calls()); n != 51 || most != 5 {
		t.Errorf("%d requests, at most %d at a time; want the listing and 50 work items, up to 5 at a time", n, most)
	}
}

func TestWorkItemShowsItsDetails(t *testing.T) {
	calls := []string{getItem}
	args := `{"project_id": "` + projectID + `", "work_item_id": "` + itemID + `"}`
	run(t, []exchange{
		{name: "paragraph tags removed from the description", tool: "plane_get_work_item", args: args, calls: calls,
			routes: map[string][]testkit.Response{getItem: {answer(`{"id":"` + itemID + `","name":"Fix login","sequence_id":12,` +
				`"identifier":"","priority":"high","state_detail":{"id":"s1","name":"Todo"},"assignees":["u1"],` +
				`"description_html":"<p>Line one</p><P>Line <b>two</b> &amp; more</P>\n  "}`)}},
			want: "Plane work item: 12 Fix login\n  id: " + itemID + "\n  state: Todo\n  assignees: u1\n  priority: high\n" +
				"  description: Line one\n<P>Line <b>two</b> &amp; more</P>"},
		{name: "a plain description clipped", tool: "plane_get_work_item", args: args, calls: calls,
			routes: map[string][]testkit.Response{getItem: {answer(`{"id":"` + itemID + `","identifier":"WEB-1","name":"n",` +
				`"description_html":"","description":"  ` + strings.Repeat("x", 1999) + `yz","state":"s2","priority":null,` +
				`"assignee_details":[{"id":"u2"}]}`)}},
			want: "Plane work item: WEB-1 n\n  id: " + itemID + "\n  state: s2\n  assignees: u2\n  priority: none\n" +
				"  description: " + strings.Repeat("x", 1999) + "y"},
		{name: "nothing known", tool: "plane_get_work_item", args: args, calls: calls,
			routes: map[string][]testkit.Response{getItem: {answer(`{"description_html":"<p></p>"}`)}},
			want:   "Plane work item: ?\n  id: ?\n  state: ?\n  assignees: none\n  priority: none"},
	})
}

func TestFailedReadsAreTriedThreeTimes(t *testing.T) {
	thrice := func(call string) []string { return []string{call, call, call} }
	failure := func(kind string, status int, target string) string {
		return fmt.Sprintf("%s error '%d %s' for url '{endpoint}/api/v1/workspaces/acme%s'\n"+
			"For more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/%d",
			kind, status, http.StatusText(status), target, status)
	}
	at := "/projects/" + projectID
	cycleArgs := `{"project_id": "` + projectID + `", "cycle_id": "` + cycleID + `"}`
	run(t, []exchange{
		{name: "projects", tool: "plane_list_projects", args: `{}`, failed: true, calls: thrice(listProjects),
			routes: map[string][]testkit.Response{listProjects: {testkit.Status(http.StatusInternalServerError)}},
			want:   failure("Server", http.StatusInternalServerError, "/projects/")},
		{name: "projects on the second attempt", tool: "plane_list_projects", args: `{}`, calls: []string{listProjects, listProjects},
			routes: map[string][]testkit.Response{listProjects: {testkit.Status(http.StatusBadGateway), answer(`[` + web + `]`)}},
			want:   webListed},
		{name: "metadata", tool: "plane_get_project_metadata", args: `{"project_id": "` + projectID + `"}`, failed: true,
			calls: slices.Concat(thrice("GET "+at+"/cycles/?cycle_view=all"), thrice("GET "+at+"/labels/"),
				thrice("GET "+at+"/project-members/"), thrice("GET "+at+"/states/")),
			routes: map[string][]testkit.Response{"GET " + at + "/states/": {testkit.Status(http.StatusForbidden)},
				"GET " + at + "/labels/": {answer(`[]`)}, "GET " + at + "/cycles/": {answer(`[]`)},
				"GET " + at + "/project-members/": {answer(`[]`)}},
			want: failure("Client", http.StatusForbidden, at+"/states/")},
		{name: "work items", tool: "plane_list_issues", args: `{"project_id": "` + projectID + `"}`, failed: true,
			calls:  thrice(listItems + "?per_page=100&expand=assignees&order_by=-created_at"),
			routes: map[string][]testkit.Response{listItems: {testkit.Status(http.StatusNotFound)}},
			want:   failure("Client", http.StatusNotFound, at+"/work-items/?per_page=100&expand=assignees&order_by=-created_at")},
		{name: "comments", tool: "plane_list_comments", failed: true,
			args:   `{"project_id": "` + projectID + `", "work_item_id": "` + itemID + `"}`,
			calls:  thrice(listComments + "?per_page=20&expand=assignees"),
			routes: map[string][]testkit.Response{listComments: {testkit.Status(http.StatusInternalServerError)}},
			want:   failure("Server", http.StatusInternalServerError, at+"/work-items/"+itemID+"/comments/?per_page=20&expand=assignees")},
		{name: "cycle", tool: "plane_list_cycle_issues", args: cycleArgs, failed: true, calls: thrice(listCycle + "?per_page=100"),
			routes: map[string][]testkit.Response{listCycle: {testkit.Status(http.StatusInternalServerError)}},
			want:   failure("Server", http.StatusInternalServerError, at+"/cycles/"+cycleID+"/cycle-issues/?per_page=100")},
		{name: "cycle on the second attempt", tool: "plane_list_cycle_issues", args: cycleArgs,
			calls: []string{listCycle + "?per_page=100", listCycle + "?per_page=100", workItemAt(uuid1)},
			routes: map[string][]testkit.Response{workItemAt(uuid1): {answer(fetched)},
				listCycle: {testkit.Status(http.StatusBadGateway), answer(`[{"id":"r","issue":"` + uuid1 + `"}]`)}},
			want: "Plane cycle issues for project_id=" + projectID + ", cycle_id=" + cycleID + ":\nTotal fetched cycle items: 1\n\n" +
				"Showing first 1 detailed rows:\n[1] " + fetchedRow},
		{name: "work item", tool: "plane_get_work_item", failed: true, calls: thrice(getItem),
			args:   `{"project_id": "` + projectID + `", "work_item_id": "` + itemID + `"}`,
			routes: map[string][]testkit.Response{getItem: {testkit.Status(http.StatusNotFound)}},
			want:   failure("Client", http.StatusNotFound, at+"/work-items/"+itemID+"/")},
	})
}
