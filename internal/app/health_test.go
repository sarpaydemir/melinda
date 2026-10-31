package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"
)

func newTestHealth(addr string, ping error, notReady ...string) *health {
	return newHealth(addr, func(context.Context) error { return ping }, func() []string { return notReady },
		slog.New(slog.DiscardHandler))
}

func get(t *testing.T, h http.Handler, path string) (int, string, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w.Code, w.Header().Get("Content-Type"), w.Body.String()
}

func TestHealthzAnswersOKEvenWhenNotReady(t *testing.T) {
	code, _, body := get(t, newTestHealth("", errors.New("connection refused"), "gateway: stopped").srv.Handler, "/healthz")
	if code != http.StatusOK || body != "ok" {
		t.Errorf("GET /healthz = %d %q, want 200 \"ok\"", code, body)
	}
}

func TestReadyzReportsTheDatabaseAndEveryComponentThatIsNotReady(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ping     error
		notReady []string
		code     int
		body     string
	}{
		{"ready", nil, nil, http.StatusOK, `{"ready":true,"failing":[]}`},
		{"component not ready", nil, []string{"flusher: starting"}, http.StatusServiceUnavailable,
			`{"ready":false,"failing":["flusher: starting"]}`},
		{"database and components", errors.New("connection refused"), []string{"gateway: not connected", "pipeline: stopped"},
			http.StatusServiceUnavailable, `{"ready":false,"failing":["database: connection refused","gateway: not connected","pipeline: stopped"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, contentType, body := get(t, newTestHealth("", tc.ping, tc.notReady...).srv.Handler, "/readyz")
			if code != tc.code || body != tc.body || contentType != "application/json" {
				t.Errorf("GET /readyz = %d %s %s, want %d application/json %s", code, contentType, body, tc.code, tc.body)
			}
		})
	}
}

func TestReadyzGivesUpOnADatabaseThatDoesNotAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hang := func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}
		begin := time.Now()
		code, _, body := get(t, newHealth("", hang, func() []string { return nil }, slog.New(slog.DiscardHandler)).srv.Handler, "/readyz")
		if code != http.StatusServiceUnavailable || body != `{"ready":false,"failing":["database: context deadline exceeded"]}` {
			t.Errorf("GET /readyz = %d %s", code, body)
		}
		if waited := time.Since(begin); waited != 2*time.Second {
			t.Errorf("waited %v for the database, want 2s", waited)
		}
	})
}

func TestHealthServesFromStartUntilStopped(t *testing.T) {
	h := newTestHealth("127.0.0.1:0", nil)
	if err := h.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()

	url := "http://" + h.ln.Addr().String() + "/healthz"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok" {
		t.Errorf("GET /healthz = %q", body)
	}

	stop()
	if err := <-done; err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
	if _, err := http.Get(url); err == nil {
		t.Error("still serving after Run returned")
	}
}

func TestHealthStartFailsWhenTheAddressIsTaken(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	if err := newTestHealth(taken.Addr().String(), nil).Start(t.Context()); err == nil {
		t.Error("Start succeeded on an address in use")
	}
}
