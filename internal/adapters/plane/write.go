package plane

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sarpaydemir/melinda/internal/port/tool"
	"github.com/sarpaydemir/melinda/internal/prompt"
	"github.com/sarpaydemir/melinda/internal/textfmt"
)

// Resolvers for the identifiers a work item refers to.
const (
	resolveState     = "Call plane_get_project_metadata first to resolve state names"
	resolveCycleName = "Call plane_get_project_metadata first to resolve cycle names"
	resolveMember    = "Call plane_get_project_metadata first to resolve member names"
	resolveLabel     = "Call plane_get_project_metadata first to resolve label names"
	resolveParent    = "Call plane_list_issues first to resolve the parent work item"
)

// maxNote is how many characters of an error a note on a partly done write keeps.
const maxNote = 120

// priorities are the priorities a work item can have.
var priorities = []string{"urgent", "high", "medium", "low", "none"}

// writer changes the workspace for the model, attributing each change to the user it answers.
type writer struct {
	reader
	texts *prompt.Catalog
}

func (w writer) createWorkItem(ctx context.Context, call tool.Call) (string, error) {
	args, err := call.Arguments()
	if err != nil {
		return "", err
	}
	ids, missing, err := required(args, "project_id", "name")
	if missing != "" || err != nil {
		return missing, err
	}
	f, err := workItemFields(args)
	if err != nil {
		return "", err
	}
	c, missing, err := w.client()
	if c == nil {
		return missing, err
	}
	if msg := cmp.Or(notUUID(ids[0], "project_id", resolveProject), check(&f)); msg != "" {
		return msg, nil
	}
	cycle := f.CycleID
	f.Name, f.CycleID = ids[1], ""
	f.AssigneeIDs, f.LabelIDs = nonEmpty(f.AssigneeIDs), nonEmpty(f.LabelIDs)
	requester := call.Requester(ctx)
	if err := call.BeforeWrite(ctx); err != nil {
		return "", err
	}
	v, err := c.CreateWorkItem(ctx, ids[0], f)
	if err != nil {
		return "", err
	}
	item, _ := v.(textfmt.Object)
	id := get(item, "id", "?")
	result := fmt.Sprintf("Plane issue created: %s%s (id=%s)", sequence(item), textfmt.Str(get(item, "name", f.Name)), textfmt.Str(id))
	if !textfmt.Truthy(id) || id == "?" {
		return result, nil
	}
	result += w.join(ctx, c, ids[0], cycle, textfmt.Str(id))
	if requester != "" {
		result += w.attribute(ctx, c, ids[0], textfmt.Str(id), w.texts.Text("plane.attr.created"), requester)
	}
	return result, nil
}

func (w writer) updateWorkItem(ctx context.Context, call tool.Call) (string, error) {
	args, err := call.Arguments()
	if err != nil {
		return "", err
	}
	ids, missing, err := required(args, "project_id", "work_item_id")
	if missing != "" || err != nil {
		return missing, err
	}
	f, err := workItemFields(args)
	if err != nil {
		return "", err
	}
	c, missing, err := w.client()
	if c == nil {
		return missing, err
	}
	msg := cmp.Or(notUUID(ids[0], "project_id", resolveProject), notUUID(ids[1], "work_item_id", resolveWorkItem), check(&f))
	if msg != "" {
		return msg, nil
	}
	changed := f.given()
	if len(changed) == 0 {
		return "Error: No fields provided to update", nil
	}
	requester := call.Requester(ctx)
	return tool.Retry(ctx, w.clk, w.log, "plane_update_issue", func() (string, error) {
		if err := call.BeforeWrite(ctx); err != nil {
			return "", err
		}
		v, err := c.UpdateWorkItem(ctx, ids[0], ids[1], f)
		if err != nil {
			return "", err
		}
		item, _ := v.(textfmt.Object)
		result := fmt.Sprintf("Plane issue updated: %s%s (id=%s)", sequence(item),
			textfmt.Str(get(item, "name", cmp.Or(f.Name, "?"))), textfmt.Str(get(item, "id", ids[1])))
		if requester != "" {
			edited := w.texts.Render("plane.attr.edited", map[string]string{"changed": w.labels(changed)})
			result += w.attribute(ctx, c, ids[0], ids[1], edited, requester)
		}
		return result, nil
	})
}

func (w writer) createComment(ctx context.Context, call tool.Call) (string, error) {
	args, err := call.Arguments()
	if err != nil {
		return "", err
	}
	ids, missing, err := required(args, "project_id", "work_item_id", "body")
	if missing == "Error: Missing body" {
		missing = "Error: Missing comment body"
	}
	if missing != "" || err != nil {
		return missing, err
	}
	c, missing, err := w.client()
	if c == nil {
		return missing, err
	}
	if msg := cmp.Or(notUUID(ids[0], "project_id", resolveProject), notUUID(ids[1], "work_item_id", resolveWorkItem)); msg != "" {
		return msg, nil
	}
	body := w.attributed(ids[2], call.Requester(ctx))
	if err := call.BeforeWrite(ctx); err != nil {
		return "", err
	}
	v, err := c.CreateComment(ctx, ids[0], ids[1], body)
	if err != nil {
		return "", err
	}
	comment, _ := v.(textfmt.Object)
	return fmt.Sprintf("Plane comment added (id=%s, work_item_id=%s)", textfmt.Str(get(comment, "id", "?")), ids[1]), nil
}

// join adds the new work item to cycle, or to the cycle of the current month when cycle is empty, and returns the
// note that tells how it went.
func (w writer) join(ctx context.Context, c *Client, project, cycle, item string) string {
	if cycle == "" {
		var err error
		if cycle, err = w.monthCycle(ctx, c, project); err != nil {
			return " (cycle lookup failed: " + textfmt.Clip(err.Error(), maxNote) + ")"
		}
		if cycle == "" {
			return " (no matching monthly cycle found)"
		}
	}
	if err := c.AddToCycle(ctx, project, cycle, item); err != nil {
		return " (failed to add to cycle " + cycle + ": " + textfmt.Clip(err.Error(), maxNote) + ")"
	}
	return " (added to cycle " + cycle + ")"
}

// monthCycle is the first of the project's cycles whose dates hold today's UTC date, else the first whose name
// holds the year and month, or "" when that cycle has no ID or none matches.
func (w writer) monthCycle(ctx context.Context, c *Client, project string) (string, error) {
	m, err := c.Metadata(ctx, project)
	if err != nil {
		return "", err
	}
	year, month, day := w.clk.Now().UTC().Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	id := func(o textfmt.Object) string {
		if v := o.Get("id"); textfmt.Truthy(v) {
			return textfmt.Str(v)
		}
		return ""
	}
	for _, v := range m.Cycles {
		o, _ := v.(textfmt.Object)
		start, okStart := date(o.Get("start_date"))
		end, okEnd := date(o.Get("end_date"))
		if okStart && okEnd && !today.Before(start) && !today.After(end) {
			return id(o), nil
		}
	}
	names := []string{
		fmt.Sprintf("%d-%02d", year, month), fmt.Sprintf("%d-%d", year, month),
		fmt.Sprintf("%d-%s", year, strings.ToLower(month.String())), fmt.Sprintf("%d-%s", year, strings.ToLower(month.String()[:3])),
	}
	for _, alias := range w.monthAliases(month) {
		names = append(names, fmt.Sprintf("%d-%s", year, alias), fmt.Sprintf("%s %d", alias, year))
	}
	for _, v := range m.Cycles {
		o, _ := v.(textfmt.Object)
		name, _ := o.Get("name").(string)
		name = textfmt.Lower(name)
		if slices.ContainsFunc(names, func(n string) bool { return strings.Contains(name, n) }) {
			return id(o), nil
		}
	}
	return "", nil
}

// monthAliases are the other names of month: line N of the catalog's plane.month_aliases lists those of month N,
// separated by commas.
func (w writer) monthAliases(month time.Month) []string {
	lines := textfmt.SplitLines(w.texts.Text("plane.month_aliases"))
	if int(month) > len(lines) {
		return nil
	}
	var aliases []string
	for _, a := range strings.Split(lines[month-1], ",") {
		if a = textfmt.Strip(a); a != "" {
			aliases = append(aliases, a)
		}
	}
	return aliases
}

// labels names the fields, joined by ", ": each by its label in the catalog's plane.field_labels, whose lines map a
// field to its label as field=label, or by itself when it has none.
func (w writer) labels(fields []string) string {
	known := map[string]string{}
	for _, line := range textfmt.SplitLines(w.texts.Text("plane.field_labels")) {
		if field, label, ok := strings.Cut(line, "="); ok {
			known[textfmt.Strip(field)] = textfmt.Strip(label)
		}
	}
	named := make([]string, len(fields))
	for i, f := range fields {
		named[i] = cmp.Or(known[f], f)
	}
	return strings.Join(named, ", ")
}

// attribute comments text, attributed to requester, on a work item and returns "" or the note that it failed.
func (w writer) attribute(ctx context.Context, c *Client, project, item, text, requester string) string {
	if _, err := c.CreateComment(ctx, project, item, w.attributed(text, requester)); err != nil {
		return " (requester comment failed: " + textfmt.Clip(err.Error(), maxNote) + ")"
	}
	return ""
}

// attributed is text followed by a blank line and the line that names requester, or text when there is none.
func (w writer) attributed(text, requester string) string {
	if requester == "" {
		return text
	}
	line := w.texts.Render("plane.attr.requester_line", map[string]string{"requested_by": requester})
	if body := textfmt.RStrip(text); body != "" {
		return body + "\n\n" + line
	}
	return line
}

// workItemFields reads the work-item fields of args: strings stripped, and lists when they are given and not null.
func workItemFields(args tool.Args) (Fields, error) {
	t, err := texts(args, "name", "description", "state_id", "cycle_id", "priority", "start_date", "target_date", "parent_id")
	if err != nil {
		return Fields{}, err
	}
	return Fields{
		Name: t[0], Description: t[1], StateID: t[2], CycleID: t[3], Priority: t[4], StartDate: t[5], TargetDate: t[6],
		ParentID: t[7], AssigneeIDs: list(args, "assignee_ids"), LabelIDs: list(args, "label_ids"),
	}, nil
}

// list is the argument key as identifiers, or nil when it is missing or null: a list gives its items as text,
// stripped, without the empty ones; a string gives itself stripped unless it is blank; anything else gives none.
func list(args tool.Args, key string) []string {
	v, ok := args.Lookup(key)
	if !ok || v == nil {
		return nil
	}
	ids := []string{}
	switch v := v.(type) {
	case []any:
		for _, item := range v {
			if s := textfmt.Strip(textfmt.Str(item)); s != "" {
				ids = append(ids, s)
			}
		}
	case string:
		if s := textfmt.Strip(v); s != "" {
			ids = append(ids, s)
		}
	}
	return ids
}

// nonEmpty is ids, or nil when there are none.
func nonEmpty(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// check is the text that rejects the first reference of f that is not a UUID, or its priority, which it lowercases,
// or "" when f is valid.
func check(f *Fields) string {
	msg := cmp.Or(
		notUUID(f.StateID, "state_id", resolveState),
		notUUID(f.CycleID, "cycle_id", resolveCycleName),
		notUUIDs(f.AssigneeIDs, "assignee_ids", resolveMember),
		notUUIDs(f.LabelIDs, "label_ids", resolveLabel),
		notUUID(f.ParentID, "parent_id", resolveParent),
	)
	if msg != "" || f.Priority == "" {
		return msg
	}
	if f.Priority = textfmt.Lower(f.Priority); !slices.Contains(priorities, f.Priority) {
		return "Error: priority must be one of: " + strings.Join(priorities, ", ")
	}
	return ""
}

// notUUIDs is the text that tells the model to resolve the identifiers ids, labelled label, with the tool resolver
// names, or "" when they are all UUIDs.
func notUUIDs(ids []string, label, resolver string) string {
	if slices.ContainsFunc(ids, func(id string) bool { return !textfmt.IsUUID(id) }) {
		return "Error: " + label + " must be Plane UUID values. " + resolver + "."
	}
	return ""
}

// given are the names of the fields f sets, in the order they are sent.
func (f Fields) given() []string {
	var names []string
	for _, field := range []struct {
		name string
		set  bool
	}{
		{"name", f.Name != ""}, {"description", f.Description != ""}, {"state_id", f.StateID != ""},
		{"assignee_ids", f.AssigneeIDs != nil}, {"label_ids", f.LabelIDs != nil}, {"cycle_id", f.CycleID != ""},
		{"priority", f.Priority != ""}, {"start_date", f.StartDate != ""}, {"target_date", f.TargetDate != ""},
		{"parent_id", f.ParentID != ""},
	} {
		if field.set {
			names = append(names, field.name)
		}
	}
	return names
}

// sequence is the work item's truthy sequence ID followed by a space, or "".
func sequence(item textfmt.Object) string {
	if v := item.Get("sequence_id"); textfmt.Truthy(v) {
		return textfmt.Str(v) + " "
	}
	return ""
}

// date is the date that v, a string, starts with as YYYY-MM-DD, with one-digit months and days allowed.
func date(v any) (time.Time, bool) {
	s, _ := v.(string)
	d, err := time.Parse("2006-1-2", textfmt.Clip(textfmt.Strip(s), 10))
	return d, err == nil
}
