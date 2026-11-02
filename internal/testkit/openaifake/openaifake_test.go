package openaifake_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sarpaydemir/melinda/internal/testkit"
	"github.com/sarpaydemir/melinda/internal/testkit/openaifake"
)

func post(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func choice(fields string) string {
	return `{"id":"chatcmpl-fake","object":"chat.completion","model":"fake","choices":[{"index":0,` + fields + `}]}`
}

func TestChatReplies(t *testing.T) {
	f := openaifake.New(t)
	tests := []struct {
		name   string
		resp   testkit.Response
		status int
		want   string
	}{
		{"text", openaifake.Text("Merhaba"), 200,
			choice(`"finish_reason":"stop","message":{"role":"assistant","content":"Merhaba"}`)},
		{"cut off", openaifake.Text("Uzun", openaifake.Finish("length"), openaifake.NativeFinish("MAX_TOKENS")), 200,
			choice(`"finish_reason":"length","native_finish_reason":"MAX_TOKENS","message":{"role":"assistant","content":"Uzun"}`)},
		{"null content and finish", openaifake.Completion(openaifake.Content(nil), openaifake.Finish("")), 200,
			choice(`"finish_reason":null,"message":{"role":"assistant","content":null}`)},
		{"tool calls", openaifake.ToolCalls(
			openaifake.ToolCall{ID: "call_1", Name: "search_web", Arguments: `{"query":"x"}`},
			openaifake.ToolCall{ID: "call_2", Name: "visit_page", Arguments: map[string]string{"url": "u"}},
		), 200, choice(`"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"search_web","arguments":"{\"query\":\"x\"}"}},
			{"id":"call_2","type":"function","function":{"name":"visit_page","arguments":{"url":"u"}}}]}`)},
		{"error payload", openaifake.ErrorPayload("rate-limited upstream", 429), 200,
			`{"error":{"message":"rate-limited upstream","code":429}}`},
		{"rate limited", openaifake.HTTPError(429, "slow down"), 429, `{"error":{"message":"slow down","code":429}}`},
	}
	for _, tt := range tests {
		f.Chat().Reply(tt.resp)
		status, body := post(t, f.URL()+"/chat/completions", `{"model":"m","messages":[]}`)
		eq, err := testkit.JSONEqual([]byte(tt.want), body)
		if status != tt.status || err != nil || !eq {
			t.Errorf("%s: %d %s (%v), want %d %s", tt.name, status, body, err, tt.status, tt.want)
		}
	}
}

func TestEmbeddings(t *testing.T) {
	f := openaifake.New(t)
	f.Embeddings().Reply(openaifake.Embedding([]float32{0.5, -1}))
	_, body := post(t, f.URL()+"/embeddings", `{"model":"e","input":"metin"}`)
	want := `{"object":"list","model":"fake","data":[{"object":"embedding","index":0,"embedding":[0.5,-1]}]}`
	if eq, err := testkit.JSONEqual([]byte(want), body); err != nil || !eq {
		t.Errorf("embedding = %s, want %s", body, want)
	}
}
