package plane_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/port/tool"
	"github.com/sarpaydemir/melinda/internal/testkit"
	"github.com/sarpaydemir/melinda/internal/testkit/planefake"
)

// Write request targets below the workspace, and the requests that read a project's metadata.
const (
	createItem    = "POST /projects/" + projectID + "/work-items/"
	commentNew    = "POST /projects/" + projectID + "/work-items/" + uuid1 + "/comments/"
	updateItem    = "PATCH /projects/" + projectID + "/work-items/" + itemID + "/"
	commentItem   = "POST /projects/" + projectID + "/work-items/" + itemID + "/comments/"
	readStates    = "GET /projects/" + projectID + "/states/"
	readLabels    = "GET /projects/" + projectID + "/labels/"
	readCycles    = "GET /projects/" + projectID + "/cycles/"
	readMembers   = "GET /projects/" + projectID + "/project-members/"
	itemsURL      = "{endpoint}/api/v1/workspaces/acme/projects/" + projectID + "/work-items/"
	requestedBy   = "— Requested by Ada Lovelace (@ada)"
	serverError   = "Server error '500 Internal Server Error' for url '"
	moreAboutCode = "'\nFor more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/"
)

// metadataRead are the requests that read the project's metadata.
var metadataRead = []string{readStates, readLabels, readCycles + "?cycle_view=all", readMembers}

// joinCycle is the request target that adds a work item to the cycle id.
func joinCycle(id string) string {
	return "POST /projects/" + projectID + "/cycles/" + id + "/cycle-issues/"
}

// withCycles are routes that create uuid1 and answer the metadata with cycles, with joining cycle join allowed.
func withCycles(cycles, join string) map[string][]testkit.Response {
	return map[string][]testkit.Response{
		createItem: {answer(`{"id":"` + uuid1 + `"}`)}, joinCycle(join): {answer(`{}`)},
		readStates: {answer(`[]`)}, readLabels: {answer(`[]`)}, readCycles: {answer(`[` + cycles + `]`)}, readMembers: {answer(`[]`)},
	}
}

// lookedUp are the requests that create the work item Task and read the project's metadata.
func lookedUp() []string { return append([]string{createItem + ` {"name":"Task"}`}, metadataRead...) }

// createdTask are the requests that create the work item Task and add it to the cycle id after reading the metadata.
func createdTask(id string) []string {
	return append(lookedUp(), joinCycle(id)+` {"issues":["`+uuid1+`"]}`)
}

func TestCreatingAWorkItemSendsTheGivenFieldsAndAttributesIt(t *testing.T) {
	created := "Plane issue created: Task (id=" + uuid1 + ")"
	run(t, []exchange{
		{name: "every field", tool: "plane_create_issue", requester: "Ada Lovelace (@ada)",
			args: `{"project_id": " ` + projectID + ` ", "name": " Fix login ", "description": "Steps:\n1. <open>", ` +
				`"state_id": "` + uuid2 + `", "assignee_ids": [" ` + uuid3 + ` ", ""], "label_ids": "` + uuid2 + `", ` +
				`"cycle_id": "` + cycleID + `", "priority": " HIGH ", "start_date": "2026-11-01", "target_date": "soon", ` +
				`"parent_id": "` + itemID + `"}`,
			routes: map[string][]testkit.Response{createItem: {answer(`{"id":"` + uuid1 + `","name":"Fix login","sequence_id":12}`)},
				joinCycle(cycleID): {answer(`{}`)}, commentNew: {answer(`{"id":"c9"}`)}},
			want: "Plane issue created: 12 Fix login (id=" + uuid1 + ") (added to cycle " + cycleID + ")",
			calls: []string{
				createItem + ` {"state":"` + uuid2 + `","assignees":["` + uuid3 + `"],"labels":["` + uuid2 + `"],` +
					`"description_html":"<p>Steps:</p><p>1. &lt;open&gt;</p>","parent":"` + itemID + `","priority":"high",` +
					`"start_date":"2026-11-01","target_date":"soon","name":"Fix login"}`,
				joinCycle(cycleID) + ` {"issues":["` + uuid1 + `"]}`,
				commentNew + ` {"comment_html":"<p>Created this work item.</p><p></p><p>` + requestedBy + `</p>"}`,
			}},
		{name: "lists and a description that hold nothing", tool: "plane_create_issue",
			args: `{"project_id": "` + projectID + `", "name": "Task", "assignee_ids": {"a": 1}, "label_ids": 5, ` +
				`"cycle_id": "` + cycleID + `", "description": "  "}`,
			routes: map[string][]testkit.Response{createItem: {answer(`{"id":"` + uuid1 + `"}`)}, joinCycle(cycleID): {answer(`{}`)}},
			want:   created + " (added to cycle " + cycleID + ")",
			calls:  []string{createItem + ` {"name":"Task"}`, joinCycle(cycleID) + ` {"issues":["` + uuid1 + `"]}`}},
		{name: "the attribution fails", tool: "plane_create_issue", requester: "u1",
			args: `{"project_id": "` + projectID + `", "name": "Task", "cycle_id": "` + cycleID + `"}`,
			routes: map[string][]testkit.Response{createItem: {answer(`{"id":"` + uuid1 + `","name":"Task"}`)},
				joinCycle(cycleID): {answer(`{}`)}, commentNew: {testkit.Status(502)}},
			want: created + " (added to cycle " + cycleID + ") (requester comment failed: Server error '502 Bad Gateway' " +
				"for url '{endpoint}/api/v1/workspaces/acme/projects/8f0e4c1a-2b3d-4e5f-9a6b-7)",
			calls: []string{createItem + ` {"name":"Task"}`, joinCycle(cycleID) + ` {"issues":["` + uuid1 + `"]}`,
				commentNew + ` {"comment_html":"<p>Created this work item.</p><p></p><p>— Requested by u1</p>"}`}},
		{name: "an answer without an id", tool: "plane_create_issue", requester: "Ada Lovelace (@ada)",
			args:   `{"project_id": "` + projectID + `", "name": "Task", "cycle_id": "` + cycleID + `"}`,
			routes: map[string][]testkit.Response{createItem: {answer(`{"name":"Named"}`)}},
			want:   "Plane issue created: Named (id=?)", calls: []string{createItem + ` {"name":"Task"}`}},
		{name: "an answer with a null id", tool: "plane_create_issue", requester: "Ada Lovelace (@ada)",
			args:   `{"project_id": "` + projectID + `", "name": "Task", "cycle_id": "` + cycleID + `"}`,
			routes: map[string][]testkit.Response{createItem: {answer(`{"id":null,"name":null,"sequence_id":"7"}`)}},
			want:   "Plane issue created: 7 None (id=None)", calls: []string{createItem + ` {"name":"Task"}`}},
		{name: "a failed create is not repeated", tool: "plane_create_issue", args: `{"project_id": "` + projectID + `", "name": "Task"}`,
			routes: map[string][]testkit.Response{createItem: {testkit.Status(500)}},
			failed: true, want: serverError + itemsURL + moreAboutCode + "500", calls: []string{createItem + ` {"name":"Task"}`}},
	})
}

func TestNewWorkItemsJoinTheCycleOfTheMonth(t *testing.T) {
	created := "Plane issue created: Task (id=" + uuid1 + ")"
	run(t, []exchange{
		{name: "whose dates hold today before any name", tool: "plane_create_issue", args: `{"project_id": "` + projectID + `", "name": "Task"}`,
			routes: withCycles(`{"id":"cy-sep","name":"2026-10 planning","start_date":"2026-09-01","end_date":"2026-09-30"},`+
				`{"id":"cy-oct","name":"Later","start_date":"2026-10-01T00:00:00Z","end_date":"2026-10-31"}`, "cy-oct"),
			want: created + " (added to cycle cy-oct)", calls: createdTask("cy-oct")},
		{name: "dates with a time and an offset", tool: "plane_create_issue", args: `{"project_id": "` + projectID + `", "name": "Task"}`,
			routes: withCycles(`{"id":"d1","start_date":"2026-1-1","end_date":"2026-11-0"},`+
				`{"id":"d2","start_date":"2026-10-31T23:00:00-05:00","end_date":"2026-10-31T01:00:00+05:00"}`, "d2"),
			want: created + " (added to cycle d2)", calls: createdTask("d2")},
		{name: "dates with one-digit parts", tool: "plane_create_issue", args: `{"project_id": "` + projectID + `", "name": "Task"}`,
			routes: withCycles(`{"id":"d1","start_date":"2026-1-1","end_date":"2026-11-1"}`, "d1"),
			want:   created + " (added to cycle d1)", calls: createdTask("d1")},
		{name: "the first whose name holds the month number", tool: "plane_create_issue", today: "2026-01-15",
			args: `{"project_id": "` + projectID + `", "name": "Task"}`,
			routes: withCycles(`{"id":"a","name":"Backlog","start_date":null,"end_date":"2026-12-31"},`+
				`{"id":"b","name":"Sprint 2026-10"},{"id":"c","name":"2026-01"}`, "b"),
			want: created + " (added to cycle b)", calls: createdTask("b")},
		{name: "a name with a month alias", tool: "plane_create_issue", today: "2026-02-10",
			args:   `{"project_id": "` + projectID + `", "name": "Task"}`,
			routes: withCycles(`{"id":"x","name":"Q1"},{"id":"y","name":"Subat 2026"}`, "y"),
			want:   created + " (added to cycle y)", calls: createdTask("y")},
		{name: "a name with the month's abbreviation", tool: "plane_create_issue", today: "2026-02-10",
			args:   `{"project_id": "` + projectID + `", "name": "Task"}`,
			routes: withCycles(`{"id":"x","name":"Q1"},{"id":"y","name":"2026-Feb release"}`, "y"),
			want:   created + " (added to cycle y)", calls: createdTask("y")},
		{name: "a name with the month's name", tool: "plane_create_issue", today: "2026-09-10",
			args:   `{"project_id": "` + projectID + `", "name": "Task"}`,
			routes: withCycles(`{"id":"y","name":"Release 2026-SEPTEMBER","start_date":"2026-10-01"}`, "y"),
			want:   created + " (added to cycle y)", calls: createdTask("y")},
		{name: "none matches", tool: "plane_create_issue", args: `{"project_id": "` + projectID + `", "name": "Task"}`,
			routes: withCycles(`{"id":"z","name":"2025-10","start_date":"bad","end_date":"2026-12-31"},`+
				`{"id":"w","name":null,"start_date":"2026-10-01","end_date":"2026-10-30"}`, "-"),
			want: created + " (no matching monthly cycle found)", calls: lookedUp()},
		{name: "the cycles cannot be read", tool: "plane_create_issue", args: `{"project_id": "` + projectID + `", "name": "Task"}`,
			routes: func() map[string][]testkit.Response {
				r := withCycles("", "-")
				r[readStates] = []testkit.Response{testkit.Status(500)}
				return r
			}(),
			want:  created + " (cycle lookup failed: " + serverError + "{endpoint}/api/v1/workspaces/acme/projects/8f0e4c1a-2b3d-4)",
			calls: lookedUp()},
		{name: "the given cycle cannot be joined", tool: "plane_create_issue",
			args:   `{"project_id": "` + projectID + `", "name": "Task", "cycle_id": "` + cycleID + `"}`,
			routes: map[string][]testkit.Response{createItem: {answer(`{"id":"` + uuid1 + `","name":"Task","sequence_id":3}`)}, joinCycle(cycleID): {testkit.Status(500)}},
			want: "Plane issue created: 3 Task (id=" + uuid1 + ") (failed to add to cycle " + cycleID + ": " + serverError +
				"{endpoint}/api/v1/workspaces/acme/projects/8f0e4c1a-2b3d-4)",
			calls: []string{createItem + ` {"name":"Task"}`, joinCycle(cycleID) + ` {"issues":["` + uuid1 + `"]}`}},
	})
}

func TestUpdatingAWorkItemSendsOnlyTheGivenFields(t *testing.T) {
	run(t, []exchange{
		{name: "some, with an emptied list", tool: "plane_update_issue", requester: "Ada Lovelace (@ada)",
			args: `{"project_id": "` + projectID + `", "work_item_id": "` + itemID + `", "state_id": "` + uuid2 + `", ` +
				`"priority": "High", "assignee_ids": [], "label_ids": null, "name": "  ", "cycle_id": "` + cycleID + `"}`,
			routes: map[string][]testkit.Response{updateItem: {answer(`{"id":"` + itemID + `","name":"Fix login","sequence_id":12}`)},
				commentItem: {answer(`{"id":"c1"}`)}},
			want: "Plane issue updated: 12 Fix login (id=" + itemID + ")",
			calls: []string{
				updateItem + ` {"state":"` + uuid2 + `","assignees":[],"cycle_id":"` + cycleID + `","priority":"high"}`,
				commentItem + ` {"comment_html":"<p>Changed: state, assignees, cycle, priority</p><p></p><p>` + requestedBy + `</p>"}`,
			}},
		{name: "all", tool: "plane_update_issue", requester: "Ada Lovelace (@ada)",
			args: `{"project_id": "` + projectID + `", "work_item_id": "` + itemID + `", "name": " New ", "description": "a\nb", ` +
				`"state_id": "` + uuid2 + `", "assignee_ids": "` + uuid3 + `", "label_ids": " ", "cycle_id": "` + cycleID + `", ` +
				`"priority": "none", "start_date": "x", "target_date": "y", "parent_id": "` + uuid1 + `"}`,
			routes: map[string][]testkit.Response{updateItem: {answer(`{"id":"other"}`)}, commentItem: {answer(`{"id":"c1"}`)}},
			want:   "Plane issue updated: New (id=other)",
			calls: []string{
				updateItem + ` {"state":"` + uuid2 + `","assignees":["` + uuid3 + `"],"labels":[],"description_html":"<p>a</p><p>b</p>",` +
					`"cycle_id":"` + cycleID + `","parent":"` + uuid1 + `","name":"New","priority":"none","start_date":"x","target_date":"y"}`,
				commentItem + ` {"comment_html":"<p>Changed: name, description, state, assignees, labels, cycle, priority, ` +
					`start date, target date, parent</p><p></p><p>` + requestedBy + `</p>"}`,
			}},
		{name: "none", tool: "plane_update_issue",
			args: `{"project_id": "` + projectID + `", "work_item_id": "` + itemID + `", "label_ids": null, "name": ""}`,
			want: "Error: No fields provided to update"},
		{name: "retried after a failure", tool: "plane_update_issue",
			args:   `{"project_id": "` + projectID + `", "work_item_id": "` + itemID + `", "priority": "low"}`,
			routes: map[string][]testkit.Response{updateItem: {testkit.Status(500), answer(`{}`)}},
			want:   "Plane issue updated: ? (id=" + itemID + ")",
			calls:  []string{updateItem + ` {"priority":"low"}`, updateItem + ` {"priority":"low"}`}},
		{name: "failing three times", tool: "plane_update_issue",
			args:   `{"project_id": "` + projectID + `", "work_item_id": "` + itemID + `", "priority": "low"}`,
			routes: map[string][]testkit.Response{updateItem: {testkit.Status(500)}},
			failed: true, want: serverError + itemsURL + itemID + "/" + moreAboutCode + "500",
			calls: []string{updateItem + ` {"priority":"low"}`, updateItem + ` {"priority":"low"}`, updateItem + ` {"priority":"low"}`}},
		{name: "the attribution fails", tool: "plane_update_issue", requester: "Ada Lovelace (@ada)",
			args:   `{"project_id": "` + projectID + `", "work_item_id": "` + itemID + `", "target_date": "2026-12-01"}`,
			routes: map[string][]testkit.Response{updateItem: {answer(`{"id":"` + itemID + `","name":"N"}`)}, commentItem: {testkit.Status(500)}},
			want: "Plane issue updated: N (id=" + itemID + ") (requester comment failed: " + serverError +
				"{endpoint}/api/v1/workspaces/acme/projects/8f0e4c1a-2b3d-4)",
			calls: []string{updateItem + ` {"target_date":"2026-12-01"}`,
				commentItem + ` {"comment_html":"<p>Changed: target date</p><p></p><p>` + requestedBy + `</p>"}`}},
	})
}

func TestCommentsCarryTheRequester(t *testing.T) {
	added := "Plane comment added (id=c1, work_item_id=" + itemID + ")"
	run(t, []exchange{
		{name: "named", tool: "plane_create_comment", requester: "Ada Lovelace (@ada)",
			args:   `{"project_id": "` + projectID + `", "work_item_id": "` + itemID + `", "body": "  Looks <good>\n\n"}`,
			routes: map[string][]testkit.Response{commentItem: {answer(`{"id":"c1"}`)}}, want: added,
			calls: []string{commentItem + ` {"comment_html":"<p>Looks &lt;good&gt;</p><p></p><p>` + requestedBy + `</p>"}`}},
		{name: "none", tool: "plane_create_comment", args: `{"project_id": "` + projectID + `", "work_item_id": "` + itemID + `", "body": "Hi"}`,
			routes: map[string][]testkit.Response{commentItem: {answer(`{}`)}},
			want:   "Plane comment added (id=?, work_item_id=" + itemID + ")", calls: []string{commentItem + ` {"comment_html":"<p>Hi</p>"}`}},
		{name: "a numeric id", tool: "plane_create_comment", requester: "u9",
			args:   `{"project_id": "` + projectID + `", "work_item_id": "` + itemID + `", "body": "Hi"}`,
			routes: map[string][]testkit.Response{commentItem: {answer(`{"id":5}`)}},
			want:   "Plane comment added (id=5, work_item_id=" + itemID + ")",
			calls:  []string{commentItem + ` {"comment_html":"<p>Hi</p><p></p><p>— Requested by u9</p>"}`}},
		{name: "a failed comment is not repeated", tool: "plane_create_comment",
			args:   `{"project_id": "` + projectID + `", "work_item_id": "` + itemID + `", "body": "Hi"}`,
			routes: map[string][]testkit.Response{commentItem: {testkit.Status(500)}},
			failed: true, want: serverError + itemsURL + itemID + "/comments/" + moreAboutCode + "500",
			calls: []string{commentItem + ` {"comment_html":"<p>Hi</p>"}`}},
	})
}

func TestWriteArgumentsAreCheckedBeforeAnyRequest(t *testing.T) {
	const (
		projectUUID = "Error: project_id must be a Plane UUID. Call plane_list_projects first to resolve the project name or identifier."
		itemUUID    = "Error: work_item_id must be a Plane UUID. Call plane_list_issues first to resolve the issue key/title."
		stateUUID   = "Error: state_id must be a Plane UUID. Call plane_get_project_metadata first to resolve state names."
		memberUUIDs = "Error: assignee_ids must be Plane UUID values. Call plane_get_project_metadata first to resolve member names."
		labelUUIDs  = "Error: label_ids must be Plane UUID values. Call plane_get_project_metadata first to resolve label names."
		parentUUID  = "Error: parent_id must be a Plane UUID. Call plane_list_issues first to resolve the parent work item."
		priority    = "Error: priority must be one of: urgent, high, medium, low, none"
		create      = "plane_create_issue"
		update      = "plane_update_issue"
		comment     = "plane_create_comment"
		p           = `"project_id": "` + projectID + `", `
		w           = `"work_item_id": "` + itemID + `", `
	)
	run(t, []exchange{
		{name: "no project", tool: create, args: `{}`, want: "Error: Missing project_id"},
		{name: "no name", tool: create, args: `{` + p + `"name": ""}`, want: "Error: Missing name"},
		{name: "a numeric name", tool: create, args: `{` + p + `"name": 5}`, failed: true, want: "'int' object has no attribute 'strip'"},
		{name: "the project before the other fields", tool: create, args: `{"name": "x", "description": 5}`, want: "Error: Missing project_id"},
		{name: "a numeric description", tool: create, args: `{` + p + `"name": "x", "description": 5}`, failed: true,
			want: "'int' object has no attribute 'strip'"},
		{name: "settings after the fields", tool: create, args: `{"project_id": "WEB", "name": "x", "parent_id": 5}`,
			settings: `{"model":"m"}`, failed: true, want: "'int' object has no attribute 'strip'"},
		{name: "settings before the references", tool: create, args: `{"project_id": "WEB", "name": "x"}`,
			settings: `{"model":"m"}`, want: "Error: Missing Plane config: plane_endpoint, plane_api_secret, plane_workspace_slug"},
		{name: "a project key", tool: create, args: `{"project_id": "WEB", "name": "x", "state_id": "Todo"}`, want: projectUUID},
		{name: "a state name", tool: create, args: `{` + p + `"name": "x", "state_id": "Todo", "priority": "asap"}`, want: stateUUID},
		{name: "a cycle name", tool: create, args: `{` + p + `"name": "x", "cycle_id": "October", "assignee_ids": ["a"]}`,
			want: "Error: cycle_id must be a Plane UUID. Call plane_get_project_metadata first to resolve cycle names."},
		{name: "a member name", tool: create, args: `{` + p + `"name": "x", "assignee_ids": ["a"], "label_ids": ["b"]}`, want: memberUUIDs},
		{name: "a null member", tool: create, args: `{` + p + `"name": "x", "assignee_ids": ["` + uuid1 + `", null]}`, want: memberUUIDs},
		{name: "a numeric label", tool: create, args: `{` + p + `"name": "x", "label_ids": [5]}`, want: labelUUIDs},
		{name: "a label name", tool: create, args: `{` + p + `"name": "x", "label_ids": "bug", "parent_id": "WEB-1"}`, want: labelUUIDs},
		{name: "a parent key", tool: create, args: `{` + p + `"name": "x", "parent_id": "WEB-1", "priority": "asap"}`, want: parentUUID},
		{name: "a priority", tool: create, args: `{` + p + `"name": "x", "priority": "asap"}`, want: priority},
		{name: "no work item", tool: update, args: `{` + p + `"name": "x"}`, want: "Error: Missing work_item_id"},
		{name: "a numeric new name", tool: update, args: `{` + p + w + `"name": 5}`, failed: true, want: "'int' object has no attribute 'strip'"},
		{name: "a work item key", tool: update, args: `{` + p + `"work_item_id": "WEB-1", "state_id": "x"}`, want: itemUUID},
		{name: "a new state name", tool: update, args: `{` + p + w + `"state_id": "x"}`, want: stateUUID},
		{name: "a null new member", tool: update, args: `{` + p + w + `"assignee_ids": [null]}`, want: memberUUIDs},
		{name: "a new parent key", tool: update, args: `{` + p + w + `"parent_id": "x", "priority": "x"}`, want: parentUUID},
		{name: "a new priority", tool: update, args: `{` + p + w + `"priority": "x"}`, want: priority},
		{name: "a blank body", tool: comment, args: `{` + p + w + `"body": " "}`, want: "Error: Missing comment body"},
		{name: "a numeric body before the project", tool: comment, args: `{` + w + `"body": 5}`, failed: true,
			want: "'int' object has no attribute 'strip'"},
		{name: "a comment in a project key", tool: comment, args: `{"project_id": "WEB", "work_item_id": "x", "body": "b"}`, want: projectUUID},
		{name: "a comment on a work item key", tool: comment, args: `{` + p + `"work_item_id": "x", "body": "b"}`, want: itemUUID},
	})
}

func TestUpdateRetriesAskBeforeEachAttempt(t *testing.T) {
	f := planefake.New(t, workspace, apiKey)
	f.Route("PATCH", "/projects/"+projectID+"/work-items/"+itemID+"/").Default(testkit.Status(500))
	refused := errors.New("the reply was abandoned")
	asked := 0
	_, err := call(t, configured(f), time.Unix(0, 0), "plane_update_issue", tool.Call{
		Args:      json.RawMessage(`{"project_id": "` + projectID + `", "work_item_id": "` + itemID + `", "priority": "low"}`),
		Requester: func(context.Context) string { return "" },
		BeforeWrite: func(context.Context) error {
			if asked++; asked > 1 {
				return refused
			}
			return nil
		},
	})
	if !errors.Is(err, refused) || len(f.Calls()) != 1 {
		t.Errorf("Call = %v after %d requests, want the refusal after the first attempt only", err, len(f.Calls()))
	}
}

func TestWritesStopWhenWritingIsRefused(t *testing.T) {
	refused := errors.New("the reply was abandoned")
	ids := `"project_id": "` + projectID + `", "work_item_id": "` + itemID + `"`
	var exchanges []exchange
	for tool, args := range map[string]string{
		"plane_create_issue":   `{"project_id": "` + projectID + `", "name": "Task"}`,
		"plane_update_issue":   `{` + ids + `, "priority": "low"}`,
		"plane_create_comment": `{` + ids + `, "body": "Hi"}`,
	} {
		exchanges = append(exchanges, exchange{name: tool, tool: tool, args: args, requester: "Ada", refuse: refused,
			failed: true, want: refused.Error()})
	}
	run(t, exchanges)
}
