package tools

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/sarpaydemir/melinda/internal/port/chat"
	"github.com/sarpaydemir/melinda/internal/port/web"
	"github.com/sarpaydemir/melinda/internal/prompt"
	"github.com/sarpaydemir/melinda/internal/testkit/conformance"
)

// down is a web search and an outbound path that always fail.
type down struct{}

func (down) Search(context.Context, string, int) ([]web.Result, error) {
	return nil, errors.New("down")
}

func (down) Send(context.Context, chat.Outgoing) (string, error) { return "", errors.New("down") }

func (down) Upload(context.Context, string, chat.File) (string, error) { return "", errors.New("down") }

func TestBuiltinToolsKeepTheToolContract(t *testing.T) {
	c, err := prompt.Load(prompt.Builtin(), "")
	if err != nil {
		t.Fatal(err)
	}
	knowledge, conversation := Builtin{Search: down{}, SearchLimit: 5, Chat: down{}, MaxProgressUpdates: 2,
		ProgressUpdateMaxChars: 500, Catalog: c, Log: slog.New(slog.DiscardHandler)}.tools()
	conformance.Tools(t, append(knowledge, conversation...), prompt.Builtin())
}
