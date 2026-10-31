package tools

import (
	"context"
	"encoding/json"

	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

// searchWeb is search_web: the first results of a web search, printed as a structured value.
type searchWeb struct{ b Builtin }

func (searchWeb) Name() string { return "search_web" }

func (searchWeb) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)
}

// Call answers a failed search as one without results.
func (s searchWeb) Call(ctx context.Context, c tool.Call) (string, error) {
	args, err := c.Arguments()
	if err != nil {
		return "", err
	}
	query, err := args.String("query", "q")
	if err != nil {
		return "", err
	}
	if query == "" {
		return "Error: Missing search query", nil
	}
	results, err := s.b.Search.Search(ctx, query, s.b.SearchLimit)
	if err != nil {
		s.b.Log.Error("web search failed", "query", query, "error", err)
		results = nil
	}
	list := make([]textfmt.Object, 0, len(results))
	for _, r := range results {
		list = append(list, textfmt.Object{{Key: "title", Value: value(r.Title)}, {Key: "url", Value: value(r.URL)},
			{Key: "content", Value: value(r.Content)}})
	}
	return textfmt.Repr(list), nil
}

// value is the text s points to, or nil for a null field.
func value(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
