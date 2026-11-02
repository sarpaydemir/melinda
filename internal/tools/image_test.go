package tools_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/port/chat"
	"github.com/sarpaydemir/melinda/internal/port/model"
	"github.com/sarpaydemir/melinda/internal/port/tool"
	"github.com/sarpaydemir/melinda/internal/prompt"
	"github.com/sarpaydemir/melinda/internal/testkit/fakeclock"
	"github.com/sarpaydemir/melinda/internal/tools"
)

// painter is an image model that answers with its images in turn, or fails with err, and records each request as
// "model/prompt".
type painter struct {
	mu       sync.Mutex
	images   []model.Image
	err      error
	requests []string
}

func (p *painter) Image(_ context.Context, name, prompt string) (model.Image, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, name+"/"+prompt)
	if p.err != nil {
		return model.Image{}, p.err
	}
	img := p.images[0]
	p.images = p.images[1:]
	return img, nil
}

// imageSet offers the tools of a reply in conversation town-square, generating images with p and model imageModel
// and uploading through o; it logs to logs and waits on clk.
func imageSet(t *testing.T, p *painter, imageModel string, o *outbound, clk *fakeclock.Clock, logs *bytes.Buffer) tools.Set {
	t.Helper()
	c, err := prompt.Load(prompt.Builtin(), "")
	if err != nil {
		t.Fatal(err)
	}
	b := tools.Builtin{Images: p, ImageModel: imageModel, Chat: o, Catalog: c, Clock: clk, Log: slog.New(slog.NewTextHandler(logs, nil))}
	set, err := tools.Offer(tool.Call{ConversationID: "town-square"}, nil, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func generateImage(set tools.Set, args string) string {
	return set.Execute(context.Background(), "generate_image", json.RawMessage(args))
}

const (
	attached     = "The image was created and attached to the reply."
	notUploaded  = "The image was created but could not be uploaded."
	imageFailed  = "The image could not be created: "
	defaultModel = "img-1"
)

func TestGenerateImageUploadsEachImageAndAttachesThemToTheReply(t *testing.T) {
	p := &painter{images: []model.Image{
		{MediaType: "image/png", Data: []byte("\x89PNG")},
		{MediaType: "image/jpeg", Data: []byte("\xff\xd8\xff")},
	}}
	o := &outbound{uploadReplies: []reply{{id: "file-a"}, {id: "file-b"}}}
	set := imageSet(t, p, defaultModel, o, fakeclock.New(time.Unix(0, 0)), &bytes.Buffer{})
	for _, args := range []string{`{"prompt": " red\n"}`, `{"prompt": "blue"}`} {
		if got := generateImage(set, args); got != attached {
			t.Errorf("%s: got %q, want %q", args, got, attached)
		}
	}
	if want := []string{"img-1/red", "img-1/blue"}; !slices.Equal(p.requests, want) {
		t.Errorf("requests %q, want %q", p.requests, want)
	}
	want := []upload{
		{"town-square", chat.File{Name: "generated_1.png", MediaType: "image/png", Data: []byte("\x89PNG")}},
		{"town-square", chat.File{Name: "generated_2.jpg", MediaType: "image/jpeg", Data: []byte("\xff\xd8\xff")}},
	}
	if !reflect.DeepEqual(o.uploads, want) {
		t.Errorf("uploaded %+v, want %+v", o.uploads, want)
	}
	if got := set.FileIDs(); !slices.Equal(got, []string{"file-a", "file-b"}) {
		t.Errorf("file IDs %q, want [file-a file-b]", got)
	}
}

func TestGenerateImageNumbersFilesByTheUploadsThatSucceeded(t *testing.T) {
	p := &painter{images: []model.Image{
		{MediaType: "image/jpeg", Data: []byte("1")},
		{MediaType: "image/png", Data: []byte("2")},
		{MediaType: "image/png", Data: []byte("3")},
	}}
	refused := errors.New("upload refused")
	o := &outbound{uploadReplies: []reply{{err: refused}, {err: refused}, {id: "file-c"}}}
	var logs bytes.Buffer
	set := imageSet(t, p, defaultModel, o, fakeclock.New(time.Unix(0, 0)), &logs)
	var results, names []string
	for range 3 {
		results = append(results, generateImage(set, `{"prompt": "a cat"}`))
	}
	for _, u := range o.uploads {
		names = append(names, u.file.Name)
	}
	if want := []string{notUploaded, notUploaded, attached}; !slices.Equal(results, want) {
		t.Errorf("results %q, want %q", results, want)
	}
	if want := []string{"generated_1.jpg", "generated_1.png", "generated_1.png"}; !slices.Equal(names, want) {
		t.Errorf("uploaded %q, want %q", names, want)
	}
	if got := set.FileIDs(); !slices.Equal(got, []string{"file-c"}) {
		t.Errorf("file IDs %q, want [file-c]", got)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 2 || !strings.Contains(logs.String(), "upload refused") {
		t.Errorf("logs %q, want each failed upload as a warning", logs.String())
	}
}

func TestGenerateImageNeedsAPromptAndAModel(t *testing.T) {
	for _, tc := range []struct{ model, args, want string }{
		{defaultModel, `{}`, imageFailed + "Missing prompt"},
		{defaultModel, `{"prompt": " \n"}`, imageFailed + "Missing prompt"},
		{"", `{"prompt": "a cat"}`, imageFailed + "image_generation_model setting is missing"},
	} {
		p, clk := &painter{}, fakeclock.New(time.Unix(0, 0))
		set := imageSet(t, p, tc.model, &outbound{}, clk, &bytes.Buffer{})
		if got := generateImage(set, tc.args); got != tc.want || len(p.requests) != 0 || clk.Timers() != 0 {
			t.Errorf("%s with model %q: got %q after %d requests; want %q at once", tc.args, tc.model, got, len(p.requests), tc.want)
		}
		if ids := set.FileIDs(); len(ids) != 0 {
			t.Errorf("file IDs %q, want none", ids)
		}
	}
}

func TestGenerateImageTriesThreeTimesBeforeReportingTheFailure(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{errors.New("API error: 500 - down"), imageFailed + "API error: 500 - down"},
		{errors.New(""), imageFailed + "Unknown error"},
	} {
		p, clk, o := &painter{err: tc.err}, fakeclock.New(time.Unix(0, 0)), &outbound{}
		set := imageSet(t, p, defaultModel, o, clk, &bytes.Buffer{})
		done := make(chan string, 1)
		go func() { done <- generateImage(set, `{"prompt": "a cat"}`) }()
		for _, wait := range []time.Duration{500 * time.Millisecond, time.Second} {
			clk.AwaitTimers(t, 1)
			clk.Advance(wait)
		}
		if got := <-done; got != tc.want || len(p.requests) != 3 || len(o.uploads) != 0 {
			t.Errorf("got %q after %d requests and %d uploads; want %q after 3 requests", got, len(p.requests), len(o.uploads), tc.want)
		}
	}
}

func TestEachReplyAttachesOnlyItsOwnImages(t *testing.T) {
	p := &painter{images: []model.Image{{MediaType: "image/png", Data: []byte("1")}, {MediaType: "image/png", Data: []byte("2")}}}
	o := &outbound{uploadReplies: []reply{{id: "file-a"}, {id: "file-b"}}}
	first := imageSet(t, p, defaultModel, o, fakeclock.New(time.Unix(0, 0)), &bytes.Buffer{})
	second := imageSet(t, p, defaultModel, o, fakeclock.New(time.Unix(0, 0)), &bytes.Buffer{})
	generateImage(first, `{"prompt": "red"}`)
	generateImage(second, `{"prompt": "blue"}`)
	if a, b := first.FileIDs(), second.FileIDs(); !slices.Equal(a, []string{"file-a"}) || !slices.Equal(b, []string{"file-b"}) {
		t.Errorf("file IDs %q and %q, want [file-a] and [file-b]", a, b)
	}
	if o.uploads[1].file.Name != "generated_1.png" {
		t.Errorf("second reply uploaded %q, want generated_1.png", o.uploads[1].file.Name)
	}
}
