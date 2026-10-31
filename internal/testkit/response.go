package testkit

import (
	"encoding/json"
	"net"
	"net/http"
	"time"
)

// Response is a scripted answer, optionally with a fault.
type Response struct {
	Status int // 200 when zero
	Header http.Header
	Body   []byte

	// Delay holds the response back; it ends early when the client gives up or the server closes.
	Delay time.Duration
	// Reset aborts the connection with a TCP reset instead of answering.
	Reset bool
	// Handler, when set, writes the response itself, for protocols such as websocket.
	Handler http.HandlerFunc
}

// JSON is a response with v encoded as JSON.
func JSON(status int, v any) Response {
	body, err := json.Marshal(v)
	if err != nil {
		panic("testkit: encode JSON response: " + err.Error())
	}
	return Raw(status, "application/json", body)
}

// Raw is a response with the given content type and body.
func Raw(status int, contentType string, body []byte) Response {
	return Response{Status: status, Header: http.Header{"Content-Type": {contentType}}, Body: body}
}

// Status is a response with an empty body.
func Status(code int) Response { return Response{Status: code} }

// ConnReset aborts the connection without answering.
func ConnReset() Response { return Response{Reset: true} }

// WithDelay returns r held back by d.
func (r Response) WithDelay(d time.Duration) Response {
	r.Delay = d
	return r
}

// Malformed returns r with its body cut in half, so it no longer parses.
func (r Response) Malformed() Response {
	if len(r.Body) < 2 {
		r.Body = []byte("{")
	} else {
		r.Body = r.Body[:len(r.Body)/2]
	}
	return r
}

func (s *Server) write(w http.ResponseWriter, r *http.Request, resp Response) {
	if resp.Delay > 0 {
		t := time.NewTimer(resp.Delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		}
	}
	if resp.Reset {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			s.t.Errorf("testkit: hijack connection for reset: %v", err)
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
		return
	}
	if resp.Handler != nil {
		resp.Handler(w, r)
		return
	}
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	if resp.Status == 0 {
		resp.Status = http.StatusOK
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}
