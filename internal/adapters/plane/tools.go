package plane

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/tool"
	"github.com/sarpaydemir/dost/internal/prompt"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

const (
	// maxCycleRows is how many listed cycle rows are read.
	maxCycleRows = 500
	// maxEnrich is how many cycle rows that name their work item have it fetched, enrichConcurrency at a time.
	maxEnrich         = 50
	enrichConcurrency = 5
)

// Resolvers name the tool that finds the identifier a UUID check rejected.
const (
	resolveProject  = "Call plane_list_projects first to resolve the project name or identifier"
	resolveWorkItem = "Call plane_list_issues first to resolve the issue key/title"
	resolveCycle    = "Call plane_get_project_metadata first to resolve cycle names or months"
)

// orderFields are the fields work items can be listed by, sorted.
var orderFields = []string{
	"completed_at", "created_at", "name", "priority", "sequence_id", "sort_order", "start_date", "target_date", "updated_at",
}

// fields are the work-item fields that creating and updating a work item take.
const fields = `"name":{"type":"string"},"description":{"type":"string"},"state_id":{"type":"string"},` +
	`"assignee_ids":{"type":"array","items":{"type":"string"}},"label_ids":{"type":"array","items":{"type":"string"}},` +
	`"cycle_id":{"type":"string"},"priority":{"type":"string","enum":["urgent","high","medium","low","none"]},` +
	`"start_date":{"type":"string"},"target_date":{"type":"string"},"parent_id":{"type":"string"}`

// Tools are the tools Plane contributes with a settings snapshot and the catalog c of their texts, in the order they
// are offered. Their requests go through transport, failed reads and updates are retried with waits on clk, and they
// log to log.
func Tools(s *config.Settings, c *prompt.Catalog, transport http.RoundTripper, clk clock.Clock, log *slog.Logger) []tool.Tool {
	r := reader{transport: transport, clk: clk, log: log}
	r.invalid = s.Decode(&r.settings)
	w := writer{r, c}
	return []tool.Tool{
		function{"plane_list_projects", `{"type":"object","properties":{}}`, r.projects},
		function{"plane_get_project_metadata", `{"type":"object","properties":{"project_id":{"type":"string"}},"required":["project_id"]}`, r.metadata},
		function{"plane_list_issues", `{"type":"object","properties":{"project_id":{"type":"string"},"query":{"type":"string"},` +
			`"state_id":{"type":"string"},"assignee_id":{"type":"string"},"cycle_id":{"type":"string"},"label_id":{"type":"string"},` +
			`"order_by":{"type":"string"},"limit":{"type":"integer"}},"required":["project_id"]}`, r.workItems},
		function{"plane_create_issue", `{"type":"object","properties":{"project_id":{"type":"string"},` + fields +
			`},"required":["project_id","name"]}`, w.createWorkItem},
		function{"plane_update_issue", `{"type":"object","properties":{"project_id":{"type":"string"},"work_item_id":{"type":"string"},` +
			fields + `},"required":["project_id","work_item_id"]}`, w.updateWorkItem},
		function{"plane_list_comments", `{"type":"object","properties":{"project_id":{"type":"string"},"work_item_id":{"type":"string"},` +
			`"limit":{"type":"integer"}},"required":["project_id","work_item_id"]}`, r.comments},
		function{"plane_list_cycle_issues", `{"type":"object","properties":{"project_id":{"type":"string"},"cycle_id":{"type":"string"},` +
			`"limit":{"type":"integer"}},"required":["project_id","cycle_id"]}`, r.cycleWorkItems},
		function{"plane_create_comment", `{"type":"object","properties":{"project_id":{"type":"string"},"work_item_id":{"type":"string"},` +
			`"body":{"type":"string"}},"required":["project_id","work_item_id","body"]}`, w.createComment},
		function{"plane_get_work_item", `{"type":"object","properties":{"project_id":{"type":"string"},"work_item_id":{"type":"string"}},` +
			`"required":["project_id","work_item_id"]}`, r.workItem},
	}
}

// Prompts are the catalog entries of Plane's tools, with their default texts.
func Prompts() []prompt.Entry {
	return []prompt.Entry{
		{ID: "tools.desc.plane_list_projects", Kind: prompt.KindRaw, Default: "List the work-tracking projects, " +
			"with their IDs and identifiers."},
		{ID: "tools.desc.plane_get_project_metadata", Kind: prompt.KindRaw, Default: "Get the states, labels, " +
			"cycles, and members of a project, with the IDs the other work-item tools take."},
		{ID: "tools.desc.plane_list_issues", Kind: prompt.KindRaw, Default: "List the work items of a project, " +
			"optionally matching a text query or filtered by state, assignee, cycle, or label ID. order_by names a " +
			"field, prefixed with - for descending order; limit is at most 50."},
		{ID: "tools.desc.plane_create_issue", Kind: prompt.KindRaw, Default: "Create a work item in a project. " +
			"Give the state, assignees, labels, cycle, and parent as IDs; priority is urgent, high, medium, low, or " +
			"none; dates are YYYY-MM-DD."},
		{ID: "tools.desc.plane_update_issue", Kind: prompt.KindRaw, Default: "Change a work item. Send only the " +
			"fields to change, with IDs as for creating one."},
		{ID: "tools.desc.plane_list_comments", Kind: prompt.KindRaw, Default: "List the comments on a work item; " +
			"limit is at most 50."},
		{ID: "tools.desc.plane_list_cycle_issues", Kind: prompt.KindRaw, Default: "List the work items in a cycle " +
			"with their state, assignees, priority, and dates; limit is at most 50."},
		{ID: "tools.desc.plane_create_comment", Kind: prompt.KindRaw, Default: "Add a comment to a work item."},
		{ID: "tools.desc.plane_get_work_item", Kind: prompt.KindRaw, Default: "Show a work item in detail: state, " +
			"assignees, priority, and description."},
		{ID: "plane.attr.requester_line", Kind: prompt.KindFormat, Placeholders: []string{"requested_by"},
			Default: "— Requested by {requested_by}"},
		{ID: "plane.attr.created", Kind: prompt.KindRaw, Default: "Created this work item."},
		{ID: "plane.attr.edited", Kind: prompt.KindFormat, Placeholders: []string{"changed"}, Default: "Changed: {changed}"},
		{ID: "plane.field_labels", Kind: prompt.KindRaw, Default: "name=name\ndescription=description\nstate_id=state\n" +
			"assignee_ids=assignees\nlabel_ids=labels\ncycle_id=cycle\npriority=priority\nstart_date=start date\n" +
			"target_date=target date\nparent_id=parent"},
		{ID: "plane.month_aliases", Kind: prompt.KindRaw, Default: ""},
	}
}

// function is a tool that runs call.
type function struct {
	name, parameters string
	call             func(context.Context, tool.Call) (string, error)
}

func (f function) Name() string { return f.name }

func (f function) Parameters() json.RawMessage { return json.RawMessage(f.parameters) }

func (f function) Call(ctx context.Context, c tool.Call) (string, error) { return f.call(ctx, c) }

// settings are Plane's keys of the settings document.
type settings struct {
	Endpoint  string `json:"plane_endpoint"`
	APIKey    string `json:"plane_api_key"`
	Workspace string `json:"plane_workspace_slug"`
}

// reader reads the workspace for the model.
type reader struct {
	settings
	// invalid is why the settings could not be read.
	invalid   error
	transport http.RoundTripper
	clk       clock.Clock
	log       *slog.Logger
}

func (r reader) projects(ctx context.Context, _ tool.Call) (string, error) {
	return r.read(ctx, "plane_list_projects", func(c *Client) (string, error) {
		p, err := c.Projects(ctx)
		if err != nil {
			return "", err
		}
		return formatProjects(p, r.Workspace), nil
	})
}

func (r reader) metadata(ctx context.Context, call tool.Call) (string, error) {
	args, err := call.Arguments()
	if err != nil {
		return "", err
	}
	ids, missing, err := required(args, "project_id")
	if missing != "" || err != nil {
		return missing, err
	}
	return r.read(ctx, "plane_get_project_metadata", func(c *Client) (string, error) {
		if msg := notUUID(ids[0], "project_id", resolveProject); msg != "" {
			return msg, nil
		}
		m, err := c.Metadata(ctx, ids[0])
		if err != nil {
			return "", err
		}
		return formatMetadata(m, ids[0]), nil
	})
}

func (r reader) workItems(ctx context.Context, call tool.Call) (string, error) {
	args, err := call.Arguments()
	if err != nil {
		return "", err
	}
	ids, missing, err := required(args, "project_id")
	if missing != "" || err != nil {
		return missing, err
	}
	n := limit(args)
	f, err := texts(args, "query", "state_id", "assignee_id", "cycle_id", "label_id", "order_by")
	if err != nil {
		return "", err
	}
	q := WorkItemQuery{Query: f[0], StateID: f[1], AssigneeID: f[2], CycleID: f[3], LabelID: f[4], OrderBy: cmp.Or(f[5], "-created_at")}
	return r.read(ctx, "plane_list_issues", func(c *Client) (string, error) {
		if msg := notUUID(ids[0], "project_id", resolveProject); msg != "" {
			return msg, nil
		}
		if !slices.Contains(orderFields, strings.TrimPrefix(q.OrderBy, "-")) {
			return "Error: order_by must be one of: " + strings.Join(orderFields, ", ") + ". Prefix with '-' for descending order.", nil
		}
		p, err := c.WorkItems(ctx, ids[0], q, n)
		if err != nil {
			return "", err
		}
		return formatWorkItems(p), nil
	})
}

func (r reader) comments(ctx context.Context, call tool.Call) (string, error) {
	args, err := call.Arguments()
	if err != nil {
		return "", err
	}
	ids, missing, err := required(args, "project_id", "work_item_id")
	if missing != "" || err != nil {
		return missing, err
	}
	n := limit(args)
	return r.read(ctx, "plane_list_comments", func(c *Client) (string, error) {
		if msg := cmp.Or(notUUID(ids[0], "project_id", resolveProject), notUUID(ids[1], "work_item_id", resolveWorkItem)); msg != "" {
			return msg, nil
		}
		p, err := c.Comments(ctx, ids[0], ids[1], n)
		if err != nil {
			return "", err
		}
		return formatComments(p, ids[1]), nil
	})
}

func (r reader) cycleWorkItems(ctx context.Context, call tool.Call) (string, error) {
	args, err := call.Arguments()
	if err != nil {
		return "", err
	}
	ids, missing, err := required(args, "project_id", "cycle_id")
	if missing != "" || err != nil {
		return missing, err
	}
	n := limit(args)
	return r.read(ctx, "plane_list_cycle_issues", func(c *Client) (string, error) {
		if msg := cmp.Or(notUUID(ids[0], "project_id", resolveProject), notUUID(ids[1], "cycle_id", resolveCycle)); msg != "" {
			return msg, nil
		}
		p, err := c.CycleWorkItems(ctx, ids[0], ids[1])
		if err != nil {
			return "", err
		}
		rows := enrich(ctx, c, ids[0], clip(p.Results, maxCycleRows))
		return formatCycleWorkItems(rows, p.TotalResults, ids[0], ids[1], n), nil
	})
}

func (r reader) workItem(ctx context.Context, call tool.Call) (string, error) {
	args, err := call.Arguments()
	if err != nil {
		return "", err
	}
	ids, missing, err := required(args, "project_id", "work_item_id")
	if missing != "" || err != nil {
		return missing, err
	}
	return r.read(ctx, "plane_get_work_item", func(c *Client) (string, error) {
		if msg := cmp.Or(notUUID(ids[0], "project_id", resolveProject), notUUID(ids[1], "work_item_id", resolveWorkItem)); msg != "" {
			return msg, nil
		}
		item, err := c.WorkItem(ctx, ids[0], ids[1])
		if err != nil {
			return "", err
		}
		return formatWorkItem(item), nil
	})
}

// read returns the text that names the missing settings, or runs read with a client for the workspace until it
// succeeds, at most three times.
func (r reader) read(ctx context.Context, name string, read func(*Client) (string, error)) (string, error) {
	c, missing, err := r.client()
	if c == nil {
		return missing, err
	}
	return tool.Retry(ctx, r.clk, r.log, name, func() (string, error) { return read(c) })
}

// client is a client for the workspace, or nil with the text that names the missing settings.
func (r reader) client() (*Client, string, error) {
	if r.invalid != nil {
		return nil, "", r.invalid
	}
	var missing []string
	for _, s := range [][2]string{{r.Endpoint, "plane_endpoint"}, {r.APIKey, "plane_api_secret"}, {r.Workspace, "plane_workspace_slug"}} {
		if s[0] == "" {
			missing = append(missing, s[1])
		}
	}
	if len(missing) > 0 {
		return nil, "Error: Missing Plane config: " + strings.Join(missing, ", "), nil
	}
	return NewClient(r.Endpoint, r.APIKey, r.Workspace, r.transport, r.log), "", nil
}

// enrich returns the cycle rows with the work items their issue holds or names: the rows that need no request keep
// their order, and the rows whose work item is fetched follow in theirs. Only the first maxEnrich rows that name one
// have it fetched, enrichConcurrency at a time.
func enrich(ctx context.Context, c *Client, project string, rows []any) []any {
	var kept []any
	var fetch []textfmt.Object
	for _, row := range rows {
		o, ok := row.(textfmt.Object)
		if !ok {
			kept = append(kept, row)
			continue
		}
		switch issue := o.Get("issue").(type) {
		case textfmt.Object:
			kept = append(kept, with(o, "work_item", issue))
		case string:
			switch {
			case !textfmt.IsUUID(textfmt.Strip(issue)):
				kept = append(kept, o)
			case len(fetch) < maxEnrich:
				fetch = append(fetch, o)
			default:
				kept = append(kept, with(o, "warning", fmt.Sprintf("Enrichment skipped for %s (limit %d); use plane_get_work_item if needed", issue, maxEnrich)))
			}
		default:
			kept = append(kept, o)
		}
	}
	fetched := make([]any, len(fetch))
	turns := make(chan struct{}, enrichConcurrency)
	var wg sync.WaitGroup
	for i, o := range fetch {
		wg.Go(func() {
			turns <- struct{}{}
			defer func() { <-turns }()
			id := o.Get("issue").(string)
			item, err := c.WorkItem(ctx, project, id)
			if err != nil {
				fetched[i] = with(o, "warning", "Could not enrich work_item_id="+id+": "+err.Error())
				return
			}
			fetched[i] = with(o, "work_item", item)
		})
	}
	wg.Wait()
	return append(kept, fetched...)
}

// with is a copy of o whose member key is v.
func with(o textfmt.Object, key string, v any) textfmt.Object {
	out := slices.DeleteFunc(slices.Clone(o), func(m textfmt.Member) bool { return m.Key == key })
	return append(out, textfmt.Member{Key: key, Value: v})
}

// texts are the string arguments names of args, in order.
func texts(args tool.Args, names ...string) ([]string, error) {
	values := make([]string, len(names))
	for i, name := range names {
		var err error
		if values[i], err = args.String(name); err != nil {
			return nil, err
		}
	}
	return values, nil
}

// required are the string arguments names of args, in order, or the text that names the first one missing.
func required(args tool.Args, names ...string) ([]string, string, error) {
	values, err := texts(args, names...)
	if err != nil {
		return nil, "", err
	}
	if i := slices.Index(values, ""); i >= 0 {
		return nil, "Error: Missing " + names[i], nil
	}
	return values, "", nil
}

// limit is the limit argument of args, 20 by default, between 1 and 50.
func limit(args tool.Args) int { return min(max(args.Int("limit", 20), 1), 50) }

// notUUID is the text that tells the model to resolve the identifier id, labelled label, with the tool resolver
// names, or "" when id is empty or a UUID.
func notUUID(id, label, resolver string) string {
	if id == "" || textfmt.IsUUID(id) {
		return ""
	}
	return "Error: " + label + " must be a Plane UUID. " + resolver + "."
}
