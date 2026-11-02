package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestCheckReady(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			t.Errorf("path = %q, want /readyz", r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	if err := checkReady(context.Background(), u.Host); err != nil {
		t.Errorf("ready server: %v", err)
	}
	status = http.StatusServiceUnavailable
	if err := checkReady(context.Background(), u.Host); err == nil {
		t.Error("503 reported as ready")
	}
	if err := checkReady(context.Background(), ""); err == nil {
		t.Error("disabled server reported as ready")
	}
	srv.Close()
	if err := checkReady(context.Background(), u.Host); err == nil {
		t.Error("closed server reported as ready")
	}
}
