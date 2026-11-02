package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/port/chat"
	"github.com/sarpaydemir/melinda/internal/routing"
	"github.com/sarpaydemir/melinda/internal/testkit"
	"github.com/sarpaydemir/melinda/internal/testkit/fakeclock"
	"github.com/sarpaydemir/melinda/internal/textfmt"
)

var (
	shadowStart = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	// platformID is the form of a Mattermost ID.
	platformID = regexp.MustCompile(`^[a-z0-9]{26}$`)
)

// unwritable is a platform whose writes fail the test.
type unwritable struct {
	chat.Platform
	t *testing.T
}

func (u unwritable) Send(context.Context, chat.Outgoing) (string, error) {
	u.t.Error("Send reached the platform")
	return "", nil
}

func (u unwritable) Upload(context.Context, string, chat.File) (string, error) {
	u.t.Error("Upload reached the platform")
	return "", nil
}

func TestShadowPlatformRecordsUploadsAndPostsWithSyntheticIDsInsteadOfWriting(t *testing.T) {
	var out bytes.Buffer
	rec := &recorder{w: &out, clk: fakeclock.New(shadowStart)}
	p := rec.platform(unwritable{t: t})
	ctx := routing.WithJob(t.Context(), routing.Job{Key: "dm:u1", MessageIDs: []string{"p1", "p2"}})

	fileID, err := p.Upload(ctx, "ch1", chat.File{Name: "generated_1.png", MediaType: "image/png", Data: []byte("png!")})
	if err != nil {
		t.Fatal(err)
	}
	postID, err := p.Send(ctx, chat.Outgoing{ConversationID: "ch1", ThreadID: "r1", Text: "Here <b>it</b> is", FileIDs: []string{fileID}})
	if err != nil {
		t.Fatal(err)
	}
	if !platformID.MatchString(fileID) || !platformID.MatchString(postID) || fileID == postID {
		t.Fatalf("IDs %q and %q are not two distinct platform IDs", fileID, postID)
	}
	want := `{"time":"2026-10-31T12:00:00Z","kind":"upload","conversation_key":"dm:u1","post_ids":["p1","p2"],"id":"` + fileID +
		`","channel_id":"ch1","file":{"name":"generated_1.png","size":4}}` + "\n" +
		`{"time":"2026-10-31T12:00:00Z","kind":"post","conversation_key":"dm:u1","post_ids":["p1","p2"],"id":"` + postID +
		`","channel_id":"ch1","root_id":"r1","message":"Here <b>it</b> is","file_ids":["` + fileID + `"]}` + "\n"
	if got := out.String(); got != want {
		t.Errorf("records:\n%s\nwant:\n%s", got, want)
	}
}

func TestShadowTransportPassesReadsAndRecordsOtherRequestsWithSyntheticAnswers(t *testing.T) {
	srv := testkit.NewServer(t)
	srv.On("GET", "/items/").Default(testkit.JSON(http.StatusOK, []string{"a"}))
	var out bytes.Buffer
	client := &http.Client{Transport: (&recorder{w: &out, clk: fakeclock.New(shadowStart)}).transport(http.DefaultTransport)}
	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL()+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, body := do("GET", "/items/", ""); code != http.StatusOK || body != `["a"]` {
		t.Errorf("GET = %d %s, want the server's answer", code, body)
	}
	code, body := do("POST", "/items/", `{"name":"x"}`)
	var created struct{ ID string }
	if err := json.Unmarshal([]byte(body), &created); err != nil || code != http.StatusCreated || !textfmt.IsUUID(created.ID) {
		t.Fatalf("POST = %d %s, want 201 with a UUID id", code, body)
	}
	if code, body := do("PATCH", "/items/1/", `{"state":"done"}`); code != http.StatusOK || body != `{}` {
		t.Errorf("PATCH = %d %s, want 200 {}", code, body)
	}
	srv.AssertCalls(t, "GET /items/")
	want := `{"time":"2026-10-31T12:00:00Z","kind":"request","id":"` + created.ID + `","method":"POST","path":"/items/","body":{"name":"x"}}` + "\n" +
		`{"time":"2026-10-31T12:00:00Z","kind":"request","method":"PATCH","path":"/items/1/","body":{"state":"done"}}` + "\n"
	if got := out.String(); got != want {
		t.Errorf("records:\n%s\nwant:\n%s", got, want)
	}
}
