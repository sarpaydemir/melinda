package files_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/sarpaydemir/dost/internal/files"
	"github.com/sarpaydemir/dost/internal/llm"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/port/model"
	"github.com/sarpaydemir/dost/internal/prompt"
)

// models answers chat requests with scripted contents, in order, and records the requests; an empty content is
// a failed answer.
type models struct {
	answers []string
	got     []model.Request
}

func (m *models) Chat(_ context.Context, r model.Request) (model.Response, error) {
	m.got = append(m.got, r)
	if len(m.answers) == 0 {
		return model.Response{}, errors.New("no answer scripted")
	}
	content := m.answers[0]
	m.answers = m.answers[1:]
	return model.Response{Message: model.Message{Role: model.RoleAssistant, Content: content}}, nil
}

// catalog is the built-in catalog with short texts for the entries a file context uses.
func catalog(t *testing.T) *prompt.Catalog {
	t.Helper()
	base, err := prompt.Load(prompt.Builtin(), "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := base.With(map[string]string{
		"files.too_many":             "[{count} more files]: skipped.",
		"files.meta_failed":          "[file {file_id}]: no details.",
		"files.kind_fallback":        "file",
		"files.unsupported":          "[{filename}]: {kind} is not supported.",
		"files.no_vision":            "[Files]: no vision model.\n",
		"files.vision":               "Describe the file.\n\nMessage: {user_message}\nFile: {file_list}",
		"files.vision_office_label":  "Extracted text:",
		"files.section":              "### {filename}\n{analysis}",
		"files.office_fallback":      "Text of the document:\n{text}",
		"files.analysis_unavailable": "[{name}]: not analysed.",
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// described is what describing a message's attachments produced.
type described struct {
	fileCtx, summary string
	requests         []model.Request
	log              string
}

func describeMessage(t *testing.T, p *platform, doc string, m chat.Message, answers ...string) described {
	t.Helper()
	var log strings.Builder
	logger := slog.New(slog.NewTextHandler(&log, nil))
	vision := &models{answers: answers}
	d := files.Describer{
		Platform: p,
		Settings: settings(t, doc),
		Catalog:  catalog(t),
		Model:    llm.Client{Provider: vision, Failure: "Failed.", Log: logger},
		Log:      logger,
	}
	fileCtx, summary := d.Describe(t.Context(), m)
	if len(vision.answers) > 0 {
		t.Errorf("unused answers %q", vision.answers)
	}
	return described{fileCtx, summary, vision.got, log.String()}
}

// asked is a vision request with one user message.
func asked(content string, parts ...model.Part) model.Request {
	return model.Request{Model: "v", Messages: []model.Message{{Role: model.RoleUser, Content: content, Parts: parts}}}
}

func visionPrompt(message, file string) string {
	return "Describe the file.\n\nMessage: " + message + "\nFile: " + file
}

func docx(t *testing.T, text string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	f, err := w.Create("word/document.xml")
	if err == nil {
		_, err = f.Write([]byte("<w:document><w:body><w:p><w:t>" + text + "</w:t></w:p></w:body></w:document>"))
	}
	if err == nil {
		err = w.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func checkDescribed(t *testing.T, got described, fileCtx, summary string, requests ...model.Request) {
	t.Helper()
	if got.fileCtx != fileCtx {
		t.Errorf("file context:\n got %q\nwant %q", got.fileCtx, fileCtx)
	}
	if got.summary != summary {
		t.Errorf("summary = %q, want %q", got.summary, summary)
	}
	if !reflect.DeepEqual(got.requests, requests) {
		t.Errorf("vision requests:\n got %+v\nwant %+v", got.requests, requests)
	}
}

func TestMixedPostIsAnalysedFileByFile(t *testing.T) {
	notes := docx(t, "Budget {approved}")
	p := &platform{files: map[string]stored{
		"f1": {meta: chat.Attachment{Name: "chart.png", Extension: "png", MediaType: "image/png", Size: 4}, body: "\x89PNG"},
		"f3": {meta: chat.Attachment{Name: "archive.zip", Extension: "zip", MediaType: "application/zip", Size: 3}},
		"f4": {meta: chat.Attachment{Name: "notes.docx", Extension: "docx", Size: int64(len(notes))}, body: string(notes)},
		"f5": {meta: chat.Attachment{Name: "report.pdf", Extension: "pdf", MediaType: "application/pdf", Size: 5}, body: "%PDF-"},
		"f6": {meta: chat.Attachment{Name: "x.png", Size: 1}, body: "x"},
		"f7": {meta: chat.Attachment{Name: "y.png", Size: 1}, body: "y"},
	}}
	m := chat.Message{Text: "u1: What is in these?", AttachmentIDs: []string{"f1", "f2", "f3", "f4", "f5", "f6", "f7"}}
	got := describeMessage(t, p, withVision, m, "A bar chart.", "", "Meeting notes.", "")
	checkDescribed(t, got,
		"[2 more files]: skipped.\n\n"+
			"### chart.png\nA bar chart.\n\n"+
			"[file f2]: no details.\n\n"+
			"[archive.zip]: .zip is not supported.\n\n"+
			"### notes.docx\nMeeting notes.\n\n"+
			"[report.pdf]: not analysed.",
		"chart.png (png), archive.zip (zip), notes.docx (docx), report.pdf (pdf)",
		asked(visionPrompt(m.Text, "chart.png"), model.Part{Filename: "chart.png", MediaType: "image/png", Data: []byte("\x89PNG")}),
		asked(visionPrompt(m.Text, "notes.docx"), model.Part{
			Filename: "notes.docx", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Data: notes,
		}),
		asked(visionPrompt(m.Text, "notes.docx")+"\n\nExtracted text:\nBudget {approved}"),
		asked(visionPrompt(m.Text, "report.pdf"), model.Part{Filename: "report.pdf", MediaType: "application/pdf", Data: []byte("%PDF-")}),
	)
}

func TestOfficeTextFallback(t *testing.T) {
	text, blank := docx(t, "Decision text"), docx(t, " ")
	request := func(data []byte) model.Request {
		return asked(visionPrompt("u1: Summarize", "k.docx"), model.Part{
			Filename: "k.docx", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Data: data,
		})
	}
	second := asked(visionPrompt("u1: Summarize", "k.docx") + "\n\nExtracted text:\nDecision text")
	for _, tc := range []struct {
		name     string
		body     []byte
		answers  []string
		fileCtx  string
		requests []model.Request
		log      string
	}{
		{"the analysis of the extracted text fails too", text, []string{"", ""},
			"### k.docx\nText of the document:\nDecision text", []model.Request{request(text), second}, ""},
		{"an answer quoting an error code starts the fallback", text, []string{"It says [ERR_DISK_FULL].", "A decision."},
			"### k.docx\nA decision.", []model.Request{request(text), second}, ""},
		{"the document cannot be read", []byte("PK"), []string{""}, "[k.docx]: not analysed.",
			[]model.Request{request([]byte("PK"))}, `filename=k.docx error="zip: not a valid zip file"`},
		{"the document has no text", blank, []string{""}, "[k.docx]: not analysed.", []model.Request{request(blank)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &platform{files: map[string]stored{"f1": {meta: chat.Attachment{Name: "k.docx"}, body: string(tc.body)}}}
			m := chat.Message{Text: "u1: Summarize", AttachmentIDs: []string{"f1"}}
			got := describeMessage(t, p, withVision, m, tc.answers...)
			checkDescribed(t, got, tc.fileCtx, "k.docx (docx)", tc.requests...)
			if tc.log == "" && got.log != "" || !strings.Contains(got.log, tc.log) {
				t.Errorf("log %q, want %q", got.log, tc.log)
			}
		})
	}
}

func TestAnswerQuotingAnErrorCodeIsTheAnalysis(t *testing.T) {
	p := &platform{files: map[string]stored{"f1": {meta: chat.Attachment{Name: "shot.png", Size: 4}, body: "\x89PNG"}}}
	m := chat.Message{Text: "u1: What does this error mean?", AttachmentIDs: []string{"f1"}}
	answer := "A chat reply: Failed. [ERR_LLM_INVOKE_FAILED_INITIAL]"
	checkDescribed(t, describeMessage(t, p, withVision, m, answer), "### shot.png\n"+answer, "shot.png (png)",
		asked(visionPrompt(m.Text, "shot.png"), model.Part{Filename: "shot.png", MediaType: "image/png", Data: []byte("\x89PNG")}))
}

func TestWithoutVisionModelAttachmentsAreOnlyNamed(t *testing.T) {
	p := &platform{files: map[string]stored{
		"f1": {meta: chat.Attachment{Name: "a.png", Size: 1}, body: "a"},
		"f2": {meta: chat.Attachment{Name: "README", Size: 1}, body: "r"},
	}}
	got := describeMessage(t, p, `{"model": "m"}`, chat.Message{Text: "u1: Look", AttachmentIDs: []string{"f1", "f9", "f2"}})
	checkDescribed(t, got, "[file f9]: no details.\n\n[Files]: no vision model.", "a.png (png), README (file)")
}

func TestMessageWithoutAttachmentsHasNoFileContext(t *testing.T) {
	got := describeMessage(t, &platform{}, withVision, chat.Message{Text: "u1: Hello"})
	checkDescribed(t, got, "", "")
}
