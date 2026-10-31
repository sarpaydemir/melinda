package tools

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/sarpaydemir/dost/internal/port/tool"
)

// builtin returns the built-in tools offered before the contributed ones and those offered after them.
func builtin() (knowledge, conversation []tool.Tool) {
	knowledge = []tool.Tool{
		declared{"search_web", `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`},
		declared{"visit_page", `{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`},
		declared{"search_public_kb", `{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`},
		declared{"generate_image", `{"type":"object","properties":{"prompt":{"type":"string"}},"required":["prompt"]}`},
	}
	conversation = []tool.Tool{
		declared{"send_progress_update", `{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`},
	}
	return knowledge, conversation
}

// declared is a tool offered by its name and argument schema; calling it is not supported yet.
type declared struct{ name, parameters string }

func (d declared) Name() string { return d.name }

func (d declared) Parameters() json.RawMessage { return json.RawMessage(d.parameters) }

func (declared) Call(context.Context, tool.Call) (string, error) { return "", errors.ErrUnsupported }
