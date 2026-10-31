package openai

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sarpaydemir/dost/internal/port/model"
)

// The wire types list their fields in the order the API receives them.

type request struct {
	Model             string      `json:"model"`
	Messages          []any       `json:"messages"`
	Temperature       json.Number `json:"temperature"`
	MaxTokens         int         `json:"max_tokens"`
	Tools             []tool      `json:"tools,omitempty"`
	ToolChoice        string      `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool       `json:"parallel_tool_calls,omitempty"`
}

type embeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type message struct {
	Role      model.Role      `json:"role"`
	Content   any             `json:"content"`
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
}

type toolResult struct {
	Role       model.Role `json:"role"`
	ToolCallID string     `json:"tool_call_id"`
	Name       string     `json:"name"`
	Content    string     `json:"content"`
}

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type imagePart struct {
	Type     string `json:"type"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

type filePart struct {
	Type string `json:"type"`
	File struct {
		Filename string `json:"filename"`
		FileData string `json:"file_data"`
	} `json:"file"`
}

type tool struct {
	Type     string   `json:"type"`
	Function function `json:"function"`
}

type function struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func (p *provider) body(r model.Request) ([]byte, error) {
	req := request{
		Model:       r.Model,
		Messages:    make([]any, 0, len(r.Messages)),
		Temperature: cmp.Or(r.Temperature, p.temperature),
		MaxTokens:   cmp.Or(r.MaxTokens, p.maxTokens),
	}
	for _, m := range r.Messages {
		w, err := wire(m)
		if err != nil {
			return nil, err
		}
		req.Messages = append(req.Messages, w)
	}
	if len(r.Tools) > 0 {
		for _, t := range r.Tools {
			req.Tools = append(req.Tools, tool{"function", function{t.Name, t.Description, t.Parameters}})
		}
		req.ToolChoice, req.ParallelToolCalls = "auto", new(false)
	}
	return encode(req)
}

// encode is v as compact UTF-8 JSON without escaping HTML or other non-ASCII characters.
func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	return unescapeSeparators(bytes.TrimSuffix(b.Bytes(), []byte("\n"))), nil
}

func wire(m model.Message) (any, error) {
	switch {
	case m.Role == model.RoleTool:
		return toolResult{m.Role, m.ToolCallID, m.Name, m.Content}, nil
	case m.Raw != nil:
		return replay(m.Raw)
	case len(m.Parts) > 0:
		return message{Role: m.Role, Content: withParts(m.Content, m.Parts)}, nil
	}
	return message{Role: m.Role, Content: m.Content}, nil
}

// replay is an assistant message the API returned, as sent back to it: its content, or "" when that is empty,
// and its tool calls, both in the API's own encoding. Other members, such as reasoning, are left out.
func replay(raw json.RawMessage) (message, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return message{}, fmt.Errorf("replay assistant message: %w", err)
	}
	m := message{Role: model.RoleAssistant, Content: ""}
	if truthy(members["content"]) {
		m.Content = members["content"]
	}
	if truthy(members["tool_calls"]) {
		m.ToolCalls = members["tool_calls"]
	}
	return m, nil
}

// withParts is a text part with text followed by a part per file: images as images, anything else as a file.
func withParts(text string, parts []model.Part) []any {
	content := []any{textPart{"text", text}}
	for _, part := range parts {
		mediaType := cmp.Or(part.MediaType, "application/octet-stream")
		url := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(part.Data)
		if strings.HasPrefix(mediaType, "image/") {
			var p imagePart
			p.Type, p.ImageURL.URL = "image_url", url
			content = append(content, p)
			continue
		}
		var p filePart
		p.Type, p.File.Filename, p.File.FileData = "file", cmp.Or(part.Filename, "file"), url
		content = append(content, p)
	}
	return content
}

// unescapeSeparators turns the \u2028 and \u2029 escapes that encoding/json writes in every string back into the
// characters. It works in place.
func unescapeSeparators(b []byte) []byte {
	out := b[:0]
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' {
			out = append(out, b[i])
			continue
		}
		switch string(b[i:min(i+6, len(b))]) {
		case `\u2028`:
			out = append(out, "\u2028"...)
			i += 5
		case `\u2029`:
			out = append(out, "\u2029"...)
			i += 5
		default: // an escaped character, or the first two bytes of another \uXXXX
			out = append(out, b[i], b[i+1])
			i++
		}
	}
	return out
}
