package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// pingTimeout bounds the database check of one readiness request.
const pingTimeout = 2 * time.Second

// health is the http component: it serves /healthz and /readyz.
type health struct {
	addr     string
	ping     func(context.Context) error
	notReady func() []string
	srv      *http.Server
	ln       net.Listener
}

// newHealth returns the component serving on addr. ping checks the database and notReady lists the components that
// are not ready; log receives the server's own errors.
func newHealth(addr string, ping func(context.Context) error, notReady func() []string, log *slog.Logger) *health {
	h := &health{addr: addr, ping: ping, notReady: notReady}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("GET /readyz", h.ready)
	h.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}
	return h
}

func (h *health) Name() string { return "http" }

// Start binds the listener, so that an address in use fails startup before later components start.
func (h *health) Start(ctx context.Context) error {
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", h.addr)
	h.ln = ln
	return err
}

func (h *health) Run(ctx context.Context) error {
	served := make(chan error, 1)
	go func() { served <- h.srv.Serve(h.ln) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	err := h.srv.Shutdown(context.WithoutCancel(ctx))
	<-served
	return err
}

func (h *health) ready(w http.ResponseWriter, r *http.Request) {
	failing := []string{}
	ctx, cancel := context.WithTimeout(r.Context(), pingTimeout)
	defer cancel()
	if err := h.ping(ctx); err != nil {
		failing = append(failing, "database: "+err.Error())
	}
	failing = append(failing, h.notReady()...)
	status := http.StatusOK
	if len(failing) > 0 {
		status = http.StatusServiceUnavailable
	}
	body, _ := json.Marshal(struct {
		Ready   bool     `json:"ready"`
		Failing []string `json:"failing"`
	}{len(failing) == 0, failing}) // a struct of a bool and strings always marshals
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
