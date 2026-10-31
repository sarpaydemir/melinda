package testkit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
)

// Request is a recorded request.
type Request struct {
	Method   string
	Path     string // escaped, as sent
	RawQuery string
	Header   http.Header
	Body     []byte
	Params   map[string]string // path parameters of the route that answered
}

// Call is "METHOD /path", followed by "?query" when there is one.
func (r Request) Call() string {
	if r.RawQuery == "" {
		return r.Method + " " + r.Path
	}
	return r.Method + " " + r.Path + "?" + r.RawQuery
}

// Query parses the raw query.
func (r Request) Query() url.Values {
	q, _ := url.ParseQuery(r.RawQuery)
	return q
}

func (r Request) String() string {
	var b strings.Builder
	b.WriteString(r.Call())
	b.WriteByte('\n')
	_ = r.Header.Write(&b)
	const limit = 2000
	if len(r.Body) > limit {
		fmt.Fprintf(&b, "\n%s… (%d bytes)", r.Body[:limit], len(r.Body))
	} else if len(r.Body) > 0 {
		fmt.Fprintf(&b, "\n%s", r.Body)
	}
	return b.String()
}

// Multipart is a parsed multipart/form-data body.
type Multipart struct {
	Values map[string][]string
	Files  []FilePart
}

// FilePart is a file in a multipart body.
type FilePart struct {
	Field, Filename, ContentType string
	Data                         []byte
}

// Multipart parses a multipart/form-data body.
func (r Request) Multipart() (Multipart, error) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		return Multipart{}, fmt.Errorf("content type %q is not multipart/form-data", r.Header.Get("Content-Type"))
	}
	m := Multipart{Values: map[string][]string{}}
	mr := multipart.NewReader(bytes.NewReader(r.Body), params["boundary"])
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return m, nil
		}
		if err != nil {
			return Multipart{}, err
		}
		data, err := io.ReadAll(p)
		if err != nil {
			return Multipart{}, err
		}
		if p.FileName() == "" {
			m.Values[p.FormName()] = append(m.Values[p.FormName()], string(data))
			continue
		}
		m.Files = append(m.Files, FilePart{
			Field: p.FormName(), Filename: p.FileName(), ContentType: p.Header.Get("Content-Type"), Data: data,
		})
	}
}
