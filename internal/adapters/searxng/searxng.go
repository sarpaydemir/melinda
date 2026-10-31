package searxng

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/web"
)

const (
	timeout = 10 * time.Second
	maxBody = 5 << 20
)

type searcher struct {
	client   *http.Client
	base     string
	language string
}

// New builds the searcher for the instance at base from one settings snapshot; transport sends its requests.
func New(s *config.Settings, base string, transport http.RoundTripper) (web.Searcher, error) {
	var c struct {
		Language string `json:"search_language"`
	}
	if err := s.Decode(&c); err != nil {
		return nil, err
	}
	return &searcher{
		client: &http.Client{
			Transport:     transport,
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		base:     strings.TrimRight(base, "/"),
		language: c.Language,
	}, nil
}

// Search asks for the JSON format, with the configured language when there is one, and safe search off.
func (s *searcher) Search(ctx context.Context, query string, limit int) ([]web.Result, error) {
	params := "q=" + url.QueryEscape(query) + "&format=json"
	if s.language != "" {
		params += "&language=" + url.QueryEscape(s.language)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/search?"+params+"&safesearch=0", nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, errors.New("answer larger than 5 MiB")
	}
	return results(body, limit)
}

// results reads the first limit hits of an answer. An answer without results has none; a hit's missing field is
// empty text.
func results(body []byte, limit int) ([]web.Result, error) {
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, err
	}
	if answer == nil {
		return nil, errors.New("answer is null")
	}
	var hits []json.RawMessage
	if raw, ok := answer["results"]; ok {
		if err := json.Unmarshal(raw, &hits); err != nil {
			return nil, err
		}
		if hits == nil {
			return nil, errors.New("results is null")
		}
	}
	out := []web.Result{}
	for i, raw := range hits {
		if i >= limit {
			break
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return nil, fmt.Errorf("result %d is not an object", i)
		}
		var r web.Result
		for _, f := range []struct {
			key string
			dst **string
		}{{"title", &r.Title}, {"url", &r.URL}, {"content", &r.Content}} {
			raw, ok := fields[f.key]
			if !ok {
				raw = json.RawMessage(`""`)
			}
			if err := json.Unmarshal(raw, f.dst); err != nil {
				return nil, fmt.Errorf("result %d: %s: %w", i, f.key, err)
			}
		}
		out = append(out, r)
	}
	return out, nil
}
