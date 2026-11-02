package webfake_test

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/testkit"
	"github.com/sarpaydemir/melinda/internal/testkit/webfake"
)

func TestPages(t *testing.T) {
	s := webfake.New(t, map[string]testkit.Response{
		"/":     webfake.HTML("<title>Ana sayfa</title><p>İçerik</p>"),
		"/old":  webfake.Redirect("/", http.StatusFound),
		"/loop": webfake.Redirect("/loop", http.StatusMovedPermanently),
		"/big":  webfake.Large(3 << 20),
		"/slow": webfake.HTML("late").WithDelay(time.Hour),
	})

	resp, err := http.Get(s.URL() + "/old")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "<title>Ana sayfa</title><p>İçerik</p>" || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("redirected page = %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if _, err := http.Get(s.URL() + "/loop"); err == nil {
		t.Error("redirect loop: want an error after too many redirects")
	}

	resp, err = http.Get(s.URL() + "/big")
	if err != nil {
		t.Fatal(err)
	}
	n, _ := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if n != 3<<20 {
		t.Errorf("large page = %d bytes, want %d", n, 3<<20)
	}

	client := &http.Client{Timeout: 50 * time.Millisecond}
	if _, err := client.Get(s.URL() + "/slow"); err == nil {
		t.Error("slow page: want a client timeout")
	}
	if calls := s.Calls(); calls[0] != "GET /old" || calls[1] != "GET /" {
		t.Errorf("calls = %v, want the redirect followed", calls)
	}
}
