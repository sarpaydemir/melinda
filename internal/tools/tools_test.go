package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/prompt"
	"github.com/sarpaydemir/dost/internal/tools"
)

// tracker is a contributed tool that reports the call it gets, or fails with err.
type tracker struct {
	name string
	err  error
}

func (tr tracker) Name() string { return tr.name }

func (tracker) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)
}

func (tr tracker) Call(ctx context.Context, c tool.Call) (string, error) {
	if tr.err != nil {
		return "", tr.err
	}
	return fmt.Sprintf("%s conversation=%s user=%s direct=%t requester=%s args=%s",
		tr.name, c.ConversationID, c.UserID, c.Direct, c.Requester(ctx), c.Args), nil
}

var contributed = []tool.Tool{tracker{name: "tracker_list"}, tracker{name: "tracker_create"}}

func catalog(t *testing.T) *prompt.Catalog {
	t.Helper()
	entries := prompt.Builtin()
	for _, tr := range contributed {
		entries = append(entries, prompt.Entry{ID: "tools.desc." + tr.Name(), Kind: prompt.KindRaw, Default: "Track work."})
	}
	c, err := prompt.Load(entries, "")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func offered(t *testing.T, set tools.Set) []string {
	t.Helper()
	var names []string
	for _, spec := range set.Specs(catalog(t)) {
		names = append(names, spec.Name)
	}
	return names
}

func TestOfferOrdersToolsAndKeepsTheOrderWhenFiltering(t *testing.T) {
	for _, c := range []struct {
		name    string
		enabled []string
		want    []string
	}{
		{"all by default", nil, []string{"search_web", "visit_page", "search_public_kb", "generate_image",
			"tracker_list", "tracker_create", "send_progress_update"}},
		{"only enabled", []string{"send_progress_update", "tracker_create", "search_web"},
			[]string{"search_web", "tracker_create", "send_progress_update"}},
		{"none enabled", []string{}, nil},
	} {
		set, err := tools.Offer(tool.Call{}, c.enabled, contributed)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := offered(t, set); !slices.Equal(got, c.want) {
			t.Errorf("%s: offered %v, want %v", c.name, got, c.want)
		}
	}
}

func TestOfferRejectsEnabledNamesOfNoOfferedTool(t *testing.T) {
	_, err := tools.Offer(tool.Call{}, []string{"search_web", "teleport", "Search_Web"}, contributed)
	want := `enabled_tools: unknown tool "teleport"` + "\n" + `enabled_tools: unknown tool "Search_Web"`
	if err == nil || err.Error() != want {
		t.Errorf("Offer = %v, want %q", err, want)
	}
}

func TestExecuteTurnsEveryFailureIntoAResult(t *testing.T) {
	failing := tracker{name: "tracker_fail", err: errors.New("tracker is down")}
	set, err := tools.Offer(tool.Call{}, []string{"tracker_list", "tracker_fail"}, append(contributed, failing))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"Tracker_List":   "Error: Unknown tool Tracker_List",
		"tracker_create": "Error: Unknown tool tracker_create",
		"":               "Error: Unknown tool ",
		"tracker_fail":   "Error: Could not execute tracker_fail: tracker is down",
	} {
		if got := set.Execute(context.Background(), name, json.RawMessage(`{}`)); got != want {
			t.Errorf("Execute(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestConcurrentRepliesEachCallWithTheirOwnContext(t *testing.T) {
	var wg sync.WaitGroup
	for reply := range 8 {
		wg.Go(func() {
			user := fmt.Sprintf("user%d", reply)
			call := tool.Call{
				ConversationID: fmt.Sprintf("conversation%d", reply),
				UserID:         user,
				Direct:         reply%2 == 0,
				Requester:      func(context.Context) string { return "Ada " + user },
			}
			set, err := tools.Offer(call, nil, contributed)
			if err != nil {
				t.Error(err)
				return
			}
			for i := range 20 {
				args := fmt.Sprintf(`{"id": "%d"}`, i)
				want := fmt.Sprintf("tracker_list conversation=conversation%d user=%s direct=%t requester=Ada %s args=%s",
					reply, user, reply%2 == 0, user, args)
				if got := set.Execute(context.Background(), "tracker_list", json.RawMessage(args)); got != want {
					t.Errorf("Execute = %q, want %q", got, want)
				}
			}
		})
	}
	wg.Wait()
}
