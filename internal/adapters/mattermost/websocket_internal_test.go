package mattermost

import "testing"

func TestWebsocketURLFollowsTheEndpoint(t *testing.T) {
	for endpoint, want := range map[string]string{
		"http://chat.example":            "ws://chat.example/api/v4/websocket",
		"https://chat.example/":          "wss://chat.example/api/v4/websocket",
		"https://chat.example//":         "wss://chat.example/api/v4/websocket",
		"http://10.0.0.5:8065/mm/":       "ws://10.0.0.5:8065/mm/api/v4/websocket",
		"http://chat.example/http://x/":  "ws://chat.example/ws://x/api/v4/websocket",
		"https://chat.example/https://x": "wss://chat.example/wss://x/api/v4/websocket",
	} {
		if got := wsURL(endpoint); got != want {
			t.Errorf("wsURL(%q) = %q, want %q", endpoint, got, want)
		}
	}
}
