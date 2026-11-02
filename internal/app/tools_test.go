package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/sarpaydemir/melinda/internal/clock"
	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/port/model"
	"github.com/sarpaydemir/melinda/internal/port/tool"
	"github.com/sarpaydemir/melinda/internal/prompt"
	"github.com/sarpaydemir/melinda/internal/testkit/golden"
	"github.com/sarpaydemir/melinda/internal/testkit/openaifake"
	"github.com/sarpaydemir/melinda/internal/tools"
)

// TestDefaultToolsAreTheEstablishedArray sends the tools offered with settings that configure no tool source,
// described by the profile in testdata/profile, and compares the tools array of the request with the one existing
// deployments send for the same descriptions.
func TestDefaultToolsAreTheEstablishedArray(t *testing.T) {
	fake := openaifake.New(t)
	fake.Chat().Reply(openaifake.Text("ok"))
	doc := map[string]json.RawMessage{
		"model": json.RawMessage(`"m"`), "api_base": json.RawMessage(`"` + fake.URL() + `"`), "key": json.RawMessage(`"k"`),
	}
	s, err := config.Load(doc, "agent", nil, func(*config.Settings) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	table := builtin(http.DefaultTransport, clock.Real(), "melinda/test", "", slog.New(slog.DiscardHandler))
	sel, err := table.choose(selection(s))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := prompt.Load(table.prompts(), filepath.Join("testdata", "profile"))
	if err != nil {
		t.Fatal(err)
	}
	set, err := tools.Offer(tool.Call{}, s.EnabledTools, tools.Builtin{}, sel.contributed(s, catalog))
	if err != nil {
		t.Fatal(err)
	}
	provider, err := sel.model(s)
	if err != nil {
		t.Fatal(err)
	}
	req := model.Request{Model: "m", Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}}, Tools: set.Specs(catalog)}
	if _, err := provider.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(fake.Requests()[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "tools", append(body["tools"], '\n'))
}
