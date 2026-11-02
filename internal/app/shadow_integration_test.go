//go:build integration

package app

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/automation"
	"github.com/sarpaydemir/melinda/internal/port/chat"
	"github.com/sarpaydemir/melinda/internal/testkit"
	"github.com/sarpaydemir/melinda/internal/testkit/mattermostfake"
	"github.com/sarpaydemir/melinda/internal/testkit/openaifake"
	"github.com/sarpaydemir/melinda/internal/testkit/planefake"
	"github.com/sarpaydemir/melinda/internal/textfmt"
)

const (
	shadowProject = "8f0e4c1a-2b3d-4e5f-9a6b-7c8d9e0f1a2b"
	shadowCycle   = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
)

var createdIssue = regexp.MustCompile(`Plane issue created: .*\(id=([0-9a-f-]+)\)`)

// shadowModel answers the issue request with a progress update, an image, and a new issue, then comments on the
// issue it was told was created, then replies; it answers anything else with a plain reply.
func shadowModel(r testkit.Request) testkit.Response {
	var body struct {
		Model    string            `json:"model"`
		Tools    []json.RawMessage `json:"tools"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(r.Body, &body)
	if body.Model == "image-model" {
		return openaifake.Completion(openaifake.Images("data:image/png;base64,iVBORw0KGgpQSVhFTFM="))
	}
	var user, results string
	for _, m := range body.Messages {
		switch m.Role {
		case "user":
			user = string(m.Content)
		case "tool":
			results += string(m.Content)
		}
	}
	if len(body.Tools) == 0 || !strings.Contains(user, "login issue") {
		return openaifake.Text("Sure.")
	}
	switch issue := createdIssue.FindStringSubmatch(results); {
	case issue == nil:
		return openaifake.ToolCalls(
			openaifake.ToolCall{ID: "call_1", Name: "send_progress_update", Arguments: `{"message":"On it"}`},
			openaifake.ToolCall{ID: "call_2", Name: "generate_image", Arguments: `{"prompt":"a login screen"}`},
			openaifake.ToolCall{ID: "call_3", Name: "plane_create_issue", Arguments: `{"project_id":"` + shadowProject + `","name":"Fix login"}`},
		)
	case !strings.Contains(results, "Plane comment added"):
		return openaifake.ToolCalls(openaifake.ToolCall{ID: "call_4", Name: "plane_create_comment",
			Arguments: `{"project_id":"` + shadowProject + `","work_item_id":"` + issue[1] + `","body":"Investigating"}`})
	}
	return openaifake.Text("Filed it.")
}

func TestShadowModeRecordsEveryWriteAndPerformsNone(t *testing.T) {
	records := filepath.Join(t.TempDir(), "shadow.jsonl")
	var plane *planefake.Fake
	var chatFake *mattermostfake.Fake
	sc := scenario{
		name: "shadow",
		settings: `"enabled_tools":["send_progress_update","generate_image","plane_create_issue","plane_create_comment"],` +
			`"image_generation_model":"image-model",` +
			`"automations":[{"name":"notify","pattern":"notify (.+)$","handler":"notify","args":{"text":1}}]`,
		setup: func(h *harness, doc map[string]json.RawMessage, table *adapters) []string {
			chatFake = h.chat
			h.model.Chat().DefaultFunc(shadowModel)
			plane = planefake.New(t, "acme", "plane-key")
			for _, list := range []string{"labels", "states", "project-members"} {
				plane.Route("GET", "/projects/{project}/"+list+"/").Default(testkit.JSON(http.StatusOK, map[string]any{"results": []any{}}))
			}
			plane.Route("GET", "/projects/{project}/cycles/").Default(testkit.JSON(http.StatusOK, map[string]any{"results": []any{
				map[string]any{"id": shadowCycle, "name": "October", "start_date": "2026-10-01", "end_date": "2026-10-31"},
			}}))
			doc["plane_endpoint"], doc["plane_api_key"], doc["plane_workspace_slug"] = raw(`"`+plane.URL()+`"`), raw(`"plane-key"`), raw(`"acme"`)
			table.handlers = maps.Clone(table.handlers)
			table.handlers["notify"] = handler{run: func(ctx context.Context, r automation.Run) (string, error) {
				_, err := r.Chat.Send(ctx, chat.Outgoing{ConversationID: r.Message.ConversationID, Text: "Noted: " + r.Args["text"]})
				return "Noted.", err
			}}
			return []string{"MELINDA_DRY_RUN=true", "MELINDA_DRY_RUN_LOG=" + records}
		},
		play: func(h *harness) {
			h.post(post("p1", "u1", "dm1", "", "please file a login issue", 0))
			h.flush(3*time.Second, 1)
			h.chat.AddPost(post("r1", "u2", "ch1", "", "kickoff", -60000))
			h.post(post("p2", "u1", "ch1", "r1", "@agent thanks", 10000))
			h.flush(3*time.Second, 2)
			h.post(post("p3", "u1", "dm1", "", "notify the team", 20000))
			h.flush(3*time.Second, 3)
		},
	}
	var doc struct {
		Posts        []string                     `json:"posts"`
		ChatSessions map[string][]json.RawMessage `json:"chat_sessions"`
	}
	if err := json.Unmarshal(play(t, sc, nil, ""), &doc); err != nil {
		t.Fatal(err)
	}
	dm, thread := len(doc.ChatSessions["melinda:"+id("u1")]), len(doc.ChatSessions["melinda:thread:"+id("r1")])
	if len(doc.Posts) != 0 || dm != 3 || thread != 1 {
		t.Errorf("posts = %q, turns saved = %d direct and %d in the thread; want no posts, 3 and 1", doc.Posts, dm, thread)
	}
	for name, calls := range map[string][]string{"chat": chatFake.Calls(), "Plane": plane.Calls()} {
		for _, c := range calls {
			if !strings.HasPrefix(c, "GET ") {
				t.Errorf("%s received %s", name, c)
			}
		}
	}
	if n := len(plane.Calls()); n != 4 {
		t.Errorf("Plane calls = %q, want the 4 reads of the project's metadata", plane.Calls())
	}

	data, err := os.ReadFile(records)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	n := 0
	for _, line := range strings.SplitAfter(strings.TrimSuffix(got, "\n"), "\n") {
		var r struct{ Kind, Method, ID string }
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("record %q: %v", line, err)
		}
		switch {
		case r.Method == "POST" && !textfmt.IsUUID(r.ID), r.Kind != "request" && !platformID.MatchString(r.ID):
			t.Errorf("record %q has no valid synthetic ID", line)
		case r.ID != "":
			n++
			got = strings.ReplaceAll(got, r.ID, "<id"+strconv.Itoa(n)+">")
		}
	}
	for _, short := range []string{"u1", "dm1", "ch1", "r1", "p1", "p2", "p3"} {
		got = strings.ReplaceAll(got, id(short), short)
	}
	const work = `/api/v1/workspaces/acme/projects/` + shadowProject
	want := `{"time":"2026-10-31T12:00:03Z","kind":"post","conversation_key":"melinda:buffer:dm:u1","post_ids":["p1"],"id":"<id1>","channel_id":"dm1","message":"On it"}
{"time":"2026-10-31T12:00:03Z","kind":"upload","conversation_key":"melinda:buffer:dm:u1","post_ids":["p1"],"id":"<id2>","channel_id":"dm1","file":{"name":"generated_1.png","size":14}}
{"time":"2026-10-31T12:00:03Z","kind":"request","conversation_key":"melinda:buffer:dm:u1","post_ids":["p1"],"id":"<id3>","method":"POST","path":"` + work + `/work-items/","body":{"name":"Fix login"}}
{"time":"2026-10-31T12:00:03Z","kind":"request","conversation_key":"melinda:buffer:dm:u1","post_ids":["p1"],"id":"<id4>","method":"POST","path":"` + work + `/cycles/` + shadowCycle + `/cycle-issues/","body":{"issues":["<id3>"]}}
{"time":"2026-10-31T12:00:03Z","kind":"request","conversation_key":"melinda:buffer:dm:u1","post_ids":["p1"],"id":"<id5>","method":"POST","path":"` + work + `/work-items/<id3>/comments/","body":{"comment_html":"<p>Created this work item.</p><p></p><p>— Requested by Ada Lovelace (@ada)</p>"}}
{"time":"2026-10-31T12:00:03Z","kind":"request","conversation_key":"melinda:buffer:dm:u1","post_ids":["p1"],"id":"<id6>","method":"POST","path":"` + work + `/work-items/<id3>/comments/","body":{"comment_html":"<p>Investigating</p><p></p><p>— Requested by Ada Lovelace (@ada)</p>"}}
{"time":"2026-10-31T12:00:03Z","kind":"post","conversation_key":"melinda:buffer:dm:u1","post_ids":["p1"],"id":"<id7>","channel_id":"dm1","message":"Filed it.","file_ids":["<id2>"]}
{"time":"2026-10-31T12:00:06Z","kind":"post","conversation_key":"melinda:buffer:thread:r1:u1","post_ids":["p2"],"id":"<id8>","channel_id":"ch1","root_id":"r1","message":"Sure."}
{"time":"2026-10-31T12:00:09Z","kind":"post","conversation_key":"melinda:buffer:dm:u1","post_ids":["p3"],"id":"<id9>","channel_id":"dm1","message":"Noted: the team"}
`
	if got != want {
		t.Errorf("records:\n%s\nwant:\n%s", got, want)
	}
}
