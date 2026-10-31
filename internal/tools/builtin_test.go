package tools

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/sarpaydemir/dost/internal/port/web"
	"github.com/sarpaydemir/dost/internal/prompt"
	"github.com/sarpaydemir/dost/internal/testkit/conformance"
)

// down is a web search that always fails.
type down struct{}

func (down) Search(context.Context, string, int) ([]web.Result, error) {
	return nil, errors.New("down")
}

func TestBuiltinToolsKeepTheToolContract(t *testing.T) {
	knowledge, conversation := Builtin{Search: down{}, SearchLimit: 5, Log: slog.New(slog.DiscardHandler)}.tools()
	conformance.Tools(t, append(knowledge, conversation...), prompt.Builtin())
}
