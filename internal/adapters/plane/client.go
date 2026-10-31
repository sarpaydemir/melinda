package plane

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/sarpaydemir/dost/internal/textfmt"
)

const (
	callTimeout = 30 * time.Second
	maxBody     = 10 << 20
	// filterWindow is how many listed work items the filters read.
	filterWindow = 300
)

// Client calls the Plane REST API of one workspace. It never retries; callers decide what is safe to repeat.
type Client struct {
	base      string
	apiKey    string
	workspace string
	http      *http.Client
	log       *slog.Logger
}

// NewClient returns a client for workspace on the server at endpoint that authenticates with apiKey, sends every
// request through transport, and logs to log the listings it does without.
func NewClient(endpoint, apiKey, workspace string, transport http.RoundTripper, log *slog.Logger) *Client {
	return &Client{
		base:      strings.TrimRight(endpoint, "/"),
		apiKey:    apiKey,
		workspace: workspace,
		http: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		log: log,
	}
}

// Page is the first page of a listing; the server's cursor to the next one is not kept. Values are as
// textfmt.Decode returns them.
type Page struct {
	Results []any
	// TotalResults is the server's total_results, or its total_count when that is falsy.
	TotalResults    any
	NextPageResults any
}

// Metadata are the states, labels, cycles, and members of a project.
type Metadata struct {
	States, Labels, Cycles, Members []any
}

// WorkItemQuery selects work items: the server sorts them by OrderBy unless it is empty, and the other fields,
// when set, filter the first page.
type WorkItemQuery struct {
	OrderBy                               string
	StateID, CycleID, AssigneeID, LabelID string
	// Query matches the name, ID, identifier, or sequence ID, ignoring case, or the number of a key like WEB-12.
	Query string
}

// Fields are the work-item fields to set. Empty strings and nil lists are not sent; an empty list is sent and
// clears the field.
type Fields struct {
	Name, Description, StateID                         string
	AssigneeIDs, LabelIDs                              []string
	CycleID, ParentID, Priority, StartDate, TargetDate string
}

// Projects lists the workspace's projects.
func (c *Client) Projects(ctx context.Context) (Page, error) {
	return c.list(ctx, "", "/projects/")
}

// Metadata fetches a project's states, labels, cycles, and members at once. Labels or cycles that cannot be
// listed are left empty, cycles only after a second request without cycle_view=all.
func (c *Client) Metadata(ctx context.Context, project string) (Metadata, error) {
	var m Metadata
	var statesErr, membersErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		p, err := c.list(ctx, "", "/projects/%s/labels/", project)
		if err != nil {
			c.log.Warn("could not list the project's labels; leaving them out", "project", project, "error", err)
		}
		m.Labels = p.Results
	})
	wg.Go(func() {
		p, err := c.list(ctx, "cycle_view=all", "/projects/%s/cycles/", project)
		if err != nil {
			c.log.Warn("could not list all of the project's cycles; asking for the default view", "project", project, "error", err)
			p, err = c.list(ctx, "", "/projects/%s/cycles/", project)
		}
		if err != nil {
			c.log.Warn("could not list the project's cycles; leaving them out", "project", project, "error", err)
		}
		m.Cycles = p.Results
	})
	wg.Go(func() {
		var p Page
		p, statesErr = c.list(ctx, "", "/projects/%s/states/", project)
		m.States = p.Results
	})
	wg.Go(func() {
		var p Page
		p, membersErr = c.list(ctx, "", "/projects/%s/project-members/", project)
		m.Members = p.Results
	})
	wg.Wait()
	if err := cmp.Or(statesErr, membersErr); err != nil {
		return Metadata{}, err
	}
	return m, nil
}

// WorkItems lists the first page of a project's work items, keeps those q selects, and returns at most limit of
// them. The total stays the server's.
func (c *Client) WorkItems(ctx context.Context, project string, q WorkItemQuery, limit int) (Page, error) {
	query := "per_page=100&expand=assignees"
	if q.OrderBy != "" {
		query += "&order_by=" + url.QueryEscape(q.OrderBy)
	}
	p, err := c.list(ctx, query, "/projects/%s/work-items/", project)
	if err != nil {
		return Page{}, err
	}
	if q.StateID != "" || q.CycleID != "" || q.AssigneeID != "" || q.LabelID != "" || q.Query != "" {
		p.Results = slices.DeleteFunc(clip(p.Results, filterWindow), func(item any) bool { return !q.selects(item) })
	}
	p.Results = clip(p.Results, limit)
	return p, nil
}

// WorkItem fetches a work item.
func (c *Client) WorkItem(ctx context.Context, project, item string) (any, error) {
	return c.call(ctx, http.MethodGet, nil, "", "/projects/%s/work-items/%s/", project, item)
}

// CreateWorkItem creates a work item and returns it.
func (c *Client) CreateWorkItem(ctx context.Context, project string, f Fields) (any, error) {
	r, s := f.parts()
	return c.call(ctx, http.MethodPost, createBody{r, s, f.Name}, "", "/projects/%s/work-items/", project)
}

// UpdateWorkItem changes the given fields of a work item and returns it.
func (c *Client) UpdateWorkItem(ctx context.Context, project, item string, f Fields) (any, error) {
	r, s := f.parts()
	return c.call(ctx, http.MethodPatch, updateBody{r, f.Name, s}, "", "/projects/%s/work-items/%s/", project, item)
}

// Comments lists the first page of a work item's comments, at most limit of them.
func (c *Client) Comments(ctx context.Context, project, item string, limit int) (Page, error) {
	query := "per_page=" + strconv.Itoa(min(limit, 50)) + "&expand=assignees"
	p, err := c.list(ctx, query, "/projects/%s/work-items/%s/comments/", project, item)
	if err != nil {
		return Page{}, err
	}
	p.Results = clip(p.Results, limit)
	return p, nil
}

// CreateComment adds a comment with text as its paragraphs and returns it.
func (c *Client) CreateComment(ctx context.Context, project, item, text string) (any, error) {
	body := struct {
		HTML string `json:"comment_html"`
	}{paragraphs(text)}
	return c.call(ctx, http.MethodPost, body, "", "/projects/%s/work-items/%s/comments/", project, item)
}

// CycleWorkItems lists the first page of a cycle's rows.
func (c *Client) CycleWorkItems(ctx context.Context, project, cycle string) (Page, error) {
	return c.list(ctx, "per_page=100", "/projects/%s/cycles/%s/cycle-issues/", project, cycle)
}

// AddToCycle adds a work item to a cycle.
func (c *Client) AddToCycle(ctx context.Context, project, cycle, item string) error {
	body := struct {
		Issues []string `json:"issues"`
	}{[]string{item}}
	_, err := c.call(ctx, http.MethodPost, body, "", "/projects/%s/cycles/%s/cycle-issues/", project, cycle)
	return err
}

// list makes a GET call and reads the answer as a listing: an array is its items; an object holds them in results
// or data and may carry totals; anything else lists nothing.
func (c *Client) list(ctx context.Context, query, format string, params ...string) (Page, error) {
	v, err := c.call(ctx, http.MethodGet, nil, query, format, params...)
	if err != nil {
		return Page{}, err
	}
	switch v := v.(type) {
	case []any:
		return Page{Results: v}, nil
	case textfmt.Object:
		results, ok := get(v, "results", get(v, "data", []any{})).([]any)
		if !ok {
			return Page{}, errors.New("the listing's results are not a list")
		}
		total := v.Get("total_results")
		if !textfmt.Truthy(total) {
			total = v.Get("total_count")
		}
		return Page{Results: results, TotalResults: total, NextPageResults: v.Get("next_page_results")}, nil
	}
	return Page{}, nil
}

// call sends body as JSON unless it is nil to the path format, under the workspace, with params escaped into it,
// and returns the decoded JSON answer.
func (c *Client) call(ctx context.Context, method string, body any, query, format string, params ...string) (any, error) {
	target, err := c.address(query, format, params)
	if err != nil {
		return nil, err
	}
	var content io.Reader
	if body != nil {
		data, err := textfmt.Encode(body)
		if err != nil {
			return nil, err
		}
		content = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, content)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, statusError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBody {
		return nil, errors.New("the answer is larger than 10 MiB")
	}
	v, err := textfmt.Decode(data, true)
	if err != nil {
		return nil, fmt.Errorf("the answer is not JSON: %w", err)
	}
	return v, nil
}

// address is the URL of format under the workspace with params escaped into it, followed by query. Empty and dot
// segments are rejected, since escaping cannot keep them from changing the path.
func (c *Client) address(query, format string, params []string) (string, error) {
	escaped := make([]any, 0, len(params)+1)
	for _, p := range append([]string{c.workspace}, params...) {
		if p == "" || p == "." || p == ".." {
			return "", fmt.Errorf("invalid path parameter %q", p)
		}
		escaped = append(escaped, url.PathEscape(p))
	}
	target := c.base + fmt.Sprintf("/api/v1/workspaces/%s"+format, escaped...)
	if query != "" {
		target += "?" + query
	}
	return target, nil
}

// statusError describes a response that is not a success, with the request's address, its password masked, and
// where a redirect points.
func statusError(resp *http.Response) error {
	kind := "Invalid status code"
	switch resp.StatusCode / 100 {
	case 1:
		kind = "Informational response"
	case 3:
		kind = "Redirect response"
	case 4:
		kind = "Client error"
	case 5:
		kind = "Server error"
	}
	text := fmt.Sprintf("%s '%s' for url '%s'\n", kind, resp.Status, resp.Request.URL.Redacted())
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		if loc, ok := resp.Header["Location"]; ok {
			text += fmt.Sprintf("Redirect location: '%s'\n", strings.Join(loc, ", "))
		}
	}
	return errors.New(text + "For more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/" +
		strconv.Itoa(resp.StatusCode))
}

// selects reports whether item passes every filter of q that is set.
func (q WorkItemQuery) selects(item any) bool {
	o, _ := item.(textfmt.Object)
	return refers(o, q.StateID, "state", "state_detail", "state_details") &&
		refers(o, q.CycleID, "cycle_id", "cycle", "cycle_detail", "cycle_details") &&
		refers(o, q.AssigneeID, "assignees", "assignee_details") &&
		refers(o, q.LabelID, "labels", "label_details") &&
		(q.Query == "" || matches(o, q.Query))
}

// refers reports whether id is empty or one of fields holds it: as the value, as an object's id, or in a list of
// either.
func refers(o textfmt.Object, id string, fields ...string) bool {
	if id == "" {
		return true
	}
	is := func(v any) bool {
		if obj, ok := v.(textfmt.Object); ok {
			v = obj.Get("id")
		}
		return v == any(id)
	}
	for _, f := range fields {
		v := o.Get(f)
		list, ok := v.([]any)
		if !ok {
			list = []any{v}
		}
		if slices.ContainsFunc(list, is) {
			return true
		}
	}
	return false
}

// matches reports whether query, lowercased and stripped, is part of the item's name, ID, identifier, or
// sequence ID, or ends in -N where N is the sequence ID.
func matches(o textfmt.Object, query string) bool {
	key := textfmt.Strip(textfmt.Lower(query))
	text := func(field string) string {
		if v := o.Get(field); textfmt.Truthy(v) {
			return textfmt.Lower(textfmt.Str(v))
		}
		return ""
	}
	seq := text("sequence_id")
	for _, field := range []string{text("name"), text("id"), text("identifier"), seq} {
		if strings.Contains(field, key) {
			return true
		}
	}
	i := strings.LastIndex(key, "-")
	if i < 0 {
		return false
	}
	n := key[i+1:]
	return n != "" && !strings.ContainsFunc(n, func(r rune) bool { return !unicode.IsDigit(r) }) && n == seq
}

// paragraphs is each line of text, escaped, in a <p> element.
func paragraphs(text string) string {
	var b strings.Builder
	for _, line := range textfmt.SplitLines(text) {
		b.WriteString("<p>" + textfmt.HTMLEscape(line) + "</p>")
	}
	return b.String()
}

// The request bodies list the renamed fields first, then the others in an order that differs by method.
type (
	renamed struct {
		State       string   `json:"state,omitzero"`
		Assignees   []string `json:"assignees,omitzero"`
		Labels      []string `json:"labels,omitzero"`
		Description string   `json:"description_html,omitzero"`
		CycleID     string   `json:"cycle_id,omitzero"`
		Parent      string   `json:"parent,omitzero"`
	}
	schedule struct {
		Priority   string `json:"priority,omitzero"`
		StartDate  string `json:"start_date,omitzero"`
		TargetDate string `json:"target_date,omitzero"`
	}
	createBody struct {
		renamed
		schedule
		Name string `json:"name,omitzero"`
	}
	updateBody struct {
		renamed
		Name string `json:"name,omitzero"`
		schedule
	}
)

// parts are f's fields other than the name, under their API names; a blank description is sent as it is.
func (f Fields) parts() (renamed, schedule) {
	description := f.Description
	if textfmt.Strip(description) != "" {
		description = paragraphs(description)
	}
	return renamed{f.StateID, f.AssigneeIDs, f.LabelIDs, description, f.CycleID, f.ParentID},
		schedule{f.Priority, f.StartDate, f.TargetDate}
}

// get is the value of o's member key, or def when o has none.
func get(o textfmt.Object, key string, def any) any {
	for _, m := range o {
		if m.Key == key {
			return m.Value
		}
	}
	return def
}

// clip is the first n items, or all of them when there are fewer.
func clip(items []any, n int) []any { return items[:max(0, min(n, len(items)))] }
