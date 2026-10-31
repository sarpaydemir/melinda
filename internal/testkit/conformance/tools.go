package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"testing"

	"github.com/sarpaydemir/dost/internal/port/tool"
	catalog "github.com/sarpaydemir/dost/internal/prompt"
)

var functionName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Tools checks the tools an adapter or the core contributes: unique function names, an object schema for the
// arguments, a raw catalog entry tools.desc.<name> among entries with a default description, and calls that return
// without panicking whatever arguments the model sends. Calls are refused any write.
func Tools(t *testing.T, tools []tool.Tool, entries []catalog.Entry) {
	t.Run("NamesAreUniqueFunctionNames", func(t *testing.T) {
		seen := map[string]bool{}
		for _, tl := range tools {
			if name := tl.Name(); !functionName.MatchString(name) || seen[name] {
				t.Errorf("name %q is not a unique function name", name)
			}
			seen[tl.Name()] = true
		}
	})

	t.Run("ParametersAreObjectSchemas", func(t *testing.T) {
		for _, tl := range tools {
			var schema struct {
				Type       string                     `json:"type"`
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			}
			if err := json.Unmarshal(tl.Parameters(), &schema); err != nil || schema.Type != "object" || schema.Properties == nil {
				t.Errorf("%s: parameters %s are not an object schema with properties (%v)", tl.Name(), tl.Parameters(), err)
				continue
			}
			for _, name := range schema.Required {
				if _, ok := schema.Properties[name]; !ok {
					t.Errorf("%s: required %q is not a property", tl.Name(), name)
				}
			}
		}
	})

	t.Run("EachHasADefaultDescription", func(t *testing.T) {
		for _, tl := range tools {
			id := "tools.desc." + tl.Name()
			i := slices.IndexFunc(entries, func(e catalog.Entry) bool { return e.ID == id })
			if i < 0 || entries[i].Kind != catalog.KindRaw || entries[i].Default == "" {
				t.Errorf("%s: no raw catalog entry %s with a default description", tl.Name(), id)
			}
		}
	})

	t.Run("CallsReturnWhateverTheArguments", func(t *testing.T) {
		call := tool.Call{
			ConversationID: "conversation",
			UserID:         "user",
			Requester:      func(context.Context) string { return "" },
			BeforeWrite:    func(context.Context) error { return errors.New("writes are refused in this check") },
		}
		for _, tl := range tools {
			for _, args := range []string{`{}`, ``, `{`, `null`, `[]`, `"text"`, `7`, `{"query": 5, "limit": "x", "id": [1]}`} {
				ctx, cancel := context.WithTimeout(t.Context(), prompt)
				call.Args = json.RawMessage(args)
				_, _ = tl.Call(ctx, call)
				cancel()
			}
		}
	})
}
