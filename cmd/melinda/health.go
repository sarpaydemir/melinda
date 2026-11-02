package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// checkReady requests /readyz on the local listener at addr, which is MELINDA_HTTP_ADDR.
func checkReady(ctx context.Context, addr string) error {
	if addr == "" {
		return errors.New("MELINDA_HTTP_ADDR is empty: the HTTP server is disabled")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("not ready: %s", resp.Status)
	}
	return nil
}
