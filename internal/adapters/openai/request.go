package openai

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sarpaydemir/melinda/internal/port/model"
	"github.com/sarpaydemir/melinda/internal/textfmt"
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

type imageRequest struct {
	Model      string    `json:"model"`
	Messages   []message `json:"messages"`
	Modalities []string  `json:"modalities"`
	MaxTokens  int       `json:"max_tokens"`
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

// body is the request's JSON. Each file's data is encoded straight into it, so the body is the only copy made.
func (p *provider) body(r model.Request) ([]byte, error) {
	f := files{mark: rand.Text()}
	req := request{
		Model:       r.Model,
		Messages:    make([]any, 0, len(r.Messages)),
		Temperature: cmp.Or(r.Temperature, p.temperature),
		MaxTokens:   cmp.Or(r.MaxTokens, p.maxTokens),
	}
	for _, m := range r.Messages {
		w, err := wire(m, &f)
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
	skeleton, err := textfmt.Encode(req)
	if err != nil {
		return nil, err
	}
	return f.splice(skeleton), nil
}

func wire(m model.Message, f *files) (any, error) {
	switch {
	case m.Role == model.RoleTool:
		return toolResult{m.Role, m.ToolCallID, m.Name, m.Content}, nil
	case m.Raw != nil:
		return replay(m.Raw)
	case len(m.Parts) > 0:
		return message{Role: m.Role, Content: withParts(m.Content, m.Parts, f)}, nil
	}
	return message{Role: m.Role, Content: m.Content}, nil
}

// replay is an assistant message the API returned, as sent back to it: its content, or "" when that is empty,
// in the API's own encoding, and its tool calls re-encoded in their original order. Other members, such as reasoning, are left out.
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
		calls, err := textfmt.Decode(members["tool_calls"], false)
		if err != nil {
			return message{}, fmt.Errorf("replay tool calls: %w", err)
		}
		if m.ToolCalls, err = textfmt.EncodeCompact(calls); err != nil {
			return message{}, fmt.Errorf("replay tool calls: %w", err)
		}
	}
	return m, nil
}

// withParts is a text part with text followed by a part per file: images as images, anything else as a file. The
// files' data is added to f, and their data URLs hold f's mark in its place.
func withParts(text string, parts []model.Part, f *files) []any {
	content := []any{textPart{"text", text}}
	for _, part := range parts {
		mediaType := cmp.Or(part.MediaType, "application/octet-stream")
		url := "data:" + mediaType + ";base64," + f.mark
		f.data = append(f.data, part.Data)
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

// files are the data of a request's file parts, in request order, and the random mark that stands in for each in
// the encoded request.
type files struct {
	mark string
	data [][]byte
}

// splice replaces the marks in skeleton, in order, with the base64 encoding of the files' data.
func (f files) splice(skeleton []byte) []byte {
	if len(f.data) == 0 {
		return skeleton
	}
	size := len(skeleton)
	for _, d := range f.data {
		size += base64.StdEncoding.EncodedLen(len(d)) - len(f.mark)
	}
	out := make([]byte, 0, size)
	for _, d := range f.data {
		before, after, _ := bytes.Cut(skeleton, []byte(f.mark))
		out = base64.StdEncoding.AppendEncode(append(out, before...), d)
		skeleton = after
	}
	return append(out, skeleton...)
}
