//go:build e2e && linux

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/testkit/mattermostfake"
	"github.com/sarpaydemir/dost/internal/testkit/openaifake"
)

func TestRapidMessagesAreAnsweredOnceTogether(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	a := e.start()
	e.post("dm1", "", "one")
	e.post("dm1", "", "two")
	e.post("dm1", "", "three")
	a.awaitReplies(e, 1)
	a.stop()

	combined := line("u1", "one") + "\n" + line("u1", "two") + "\n" + line("u1", "three")
	if got := e.modelRequests(); len(got) != 1 || got[0].last() != combined {
		t.Errorf("model requests %q, want one with %q", got, combined)
	}
	if got := e.replies(); len(got) != 1 || got[0] != (reply{ChannelID: id("dm1"), Message: "Answer to " + combined}) {
		t.Errorf("replies %+v, want one answer in the DM", got)
	}
	e.assertSettled()
}

func TestUnaddressedAndOwnPostsAreNotAnswered(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	a := e.start()
	e.chat.Posted(mattermostfake.Post{UserID: id("bot"), ChannelID: id("dm1"), Message: "my own post"})
	e.post("ch1", "", "hello everyone")
	// Events are handled in order, so once this is answered the posts before it have been handled too.
	e.post("dm1", "", "ping")
	a.awaitReplies(e, 1)
	a.stop()

	if got := e.modelRequests(); len(got) != 1 || got[0].last() != line("u1", "ping") {
		t.Errorf("model requests %q, want only the one for ping", got)
	}
	if got := e.replies(); len(got) != 1 || got[0].ChannelID != id("dm1") {
		t.Errorf("replies %+v, want only the DM answer", got)
	}
	e.assertSettled()
}

func TestBotIDOfAnotherAccountStopsStartupBeforeAnyReply(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"mattermost_bot_id": id("u1")})
	out := &logs{}
	cmd := e.command()
	cmd.Stdout, cmd.Stderr = out, out

	if err := cmd.Run(); err == nil || !strings.Contains(out.String(), id("bot")) {
		t.Errorf("exit %v, want a failure naming the token's account; logs:\n%s", err, out)
	}
	if got := e.replies(); len(got) != 0 {
		t.Errorf("replies %+v, want none", got)
	}
}

func TestMessageBufferedAtACrashIsAnsweredOnceAfterRestart(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	a := e.start()
	e.post("dm1", "", "before the crash")
	a.await("the buffered message", func() bool { return e.buffers() == 1 })
	a.kill()
	if got := e.replies(); len(got) != 0 {
		t.Fatalf("answered %+v before the kill; the debounce window was missed", got)
	}

	b := e.start()
	got := b.awaitReplies(e, 1)
	b.stop()
	if want := (reply{ChannelID: id("dm1"), Message: "Answer to " + line("u1", "before the crash")}); len(e.replies()) != 1 || got[0] != want {
		t.Errorf("replies %+v, want one: %+v", e.replies(), want)
	}
	if n := len(e.modelRequests()); n != 1 {
		t.Errorf("model requests = %d, want 1", n)
	}
	e.assertSettled()
}

func TestPostsMissedWhileDisconnectedAreAnsweredOnceAfterReconnecting(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	a := e.start()
	e.post("dm1", "", "live")
	a.awaitReplies(e, 1)

	// Stored but never pushed: the agent can only learn of these by replaying after it reconnects.
	e.chat.AddPost(mattermostfake.Post{UserID: id("u1"), ChannelID: id("dm1"), Message: "missed"})
	mention := e.chat.AddPost(mattermostfake.Post{UserID: id("u1"), ChannelID: id("ch1"), Message: "@agent missed mention"})
	e.chat.Drop()
	a.awaitReplies(e, 3)
	a.stop()

	want := map[reply]bool{
		{ChannelID: id("dm1"), Message: "Answer to " + line("u1", "live")}:                                      true,
		{ChannelID: id("dm1"), Message: "Answer to " + line("u1", "missed")}:                                    true,
		{ChannelID: id("ch1"), RootID: mention.ID, Message: "Answer to " + line("u1", "@agent missed mention")}: true,
	}
	got := e.replies()
	for _, r := range got {
		if !want[r] {
			t.Errorf("unexpected reply %+v", r)
		}
		delete(want, r)
	}
	if len(want) > 0 {
		t.Errorf("missing replies %+v; got %+v", want, got)
	}
	e.assertSettled()
}

func TestSIGTERMDuringAReplyLetsItFinish(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	e.model.Chat().Reply(openaifake.Text("Slow answer.").WithDelay(2 * time.Second))
	a := e.start()
	e.post("dm1", "", "take your time")
	a.await("the model request", func() bool { return len(e.modelRequests()) == 1 })
	signalled := time.Now()
	a.stop()
	if took := time.Since(signalled); took > shutdownTimeout {
		t.Errorf("shutdown took %v, longer than the %v timeout", took, shutdownTimeout)
	}

	if got := e.replies(); len(got) != 1 || got[0].Message != "Slow answer." {
		t.Errorf("replies %+v, want the slow answer", got)
	}
	if n := e.count(`SELECT count(*) FROM chat_sessions WHERE value::text LIKE '%Slow answer.%'`); n != 1 {
		t.Errorf("saved turns with the answer = %d, want 1", n)
	}
	e.assertSettled()
}

func TestSettingsEditAppliesToTheNextMessage(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	a := e.start()
	e.post("dm1", "", "first")
	a.awaitReplies(e, 1)
	e.writeSettings(map[string]any{"model": "edited-model"})
	e.post("dm1", "", "second")
	a.awaitReplies(e, 2)
	a.stop()

	got := e.modelRequests()
	if len(got) != 2 || got[0].Model != "chat-model" || got[1].Model != "edited-model" {
		t.Errorf("model requests %+v, want chat-model then edited-model", got)
	}
	if n := e.chat.Connections(); n != 1 {
		t.Errorf("websocket connections = %d, want 1", n)
	}
	e.assertSettled()
}

func TestModelFailureSendsTheConfiguredErrorReply(t *testing.T) {
	t.Parallel()
	e := newEnv(t, map[string]any{"prompts": map[string]string{"error.generic": "The model is unavailable."}})
	e.model.Chat().Default(openaifake.HTTPError(500, "upstream"))
	a := e.start()
	e.post("dm1", "", "hello")
	got := a.awaitReplies(e, 1)
	a.stop()

	want := "The model is unavailable. [ERR_LLM_INVOKE_FAILED_INITIAL]"
	if len(e.replies()) != 1 || got[0].Message != want {
		t.Errorf("replies %+v, want one: %q", e.replies(), want)
	}
	e.assertSettled()
}
