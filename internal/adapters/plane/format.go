package plane

import (
	"cmp"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"

	"github.com/sarpaydemir/melinda/internal/textfmt"
)

// The formatters read an item that is not an object as an object without members.

func formatProjects(p Page, workspace string) string {
	if len(p.Results) == 0 {
		return "No Plane projects found."
	}
	lines := []string{"Plane projects:"}
	for i, v := range p.Results {
		o, _ := v.(textfmt.Object)
		identifier := firstTruthy(o.Get("identifier"), o.Get("short_name"), o.Get("slug"), "?")
		lines = append(lines, fmt.Sprintf("[%d] %s (id=%s, identifier=%s, workspace=%s)",
			i+1, field(o, "name"), field(o, "id"), textfmt.Str(identifier), workspace))
	}
	return strings.Join(append(lines, listNote(p, len(p.Results))...), "\n")
}

func formatMetadata(m Metadata, project string) string {
	lines := []string{"Plane metadata for project_id=" + project + "\n"}
	section := func(title string, items []any, line func(textfmt.Object) string) {
		if len(items) == 0 {
			return
		}
		lines = append(lines, title)
		for _, v := range items {
			o, _ := v.(textfmt.Object)
			lines = append(lines, line(o))
		}
		lines = append(lines, "")
	}
	// name is the member name of o, or its id when it has none.
	name := func(o textfmt.Object) string { return textfmt.Str(get(o, "name", get(o, "id", "?"))) }
	section("States:", m.States, func(o textfmt.Object) string {
		return "- " + name(o) + " (id=" + field(o, "id") + stateDetails(o) + ")"
	})
	section("Labels:", m.Labels, func(o textfmt.Object) string {
		return "- " + name(o) + " (id=" + field(o, "id") + ")"
	})
	section("Cycles:", m.Cycles, func(o textfmt.Object) string {
		return fmt.Sprintf("- %s (id=%s, starts=%s, ends=%s)", name(o), field(o, "id"),
			textfmt.Str(firstTruthy(o.Get("start_date"), "?")), textfmt.Str(firstTruthy(o.Get("end_date"), "?")))
	})
	section("Members:", m.Members, func(o textfmt.Object) string {
		user := o
		if v := o.Get("member"); textfmt.Truthy(v) {
			user, _ = v.(textfmt.Object)
		}
		display := firstTruthy(user.Get("display_name"), get(user, "first_name", ""))
		text := textfmt.Str(display)
		if username := user.Get("username"); textfmt.Truthy(display) && textfmt.Truthy(username) {
			text += " (@" + textfmt.Str(username) + ")"
		}
		return "- " + text + " (id=" + textfmt.Str(get(user, "id", get(o, "id", "?"))) + ")"
	})
	return textfmt.Strip(strings.Join(lines, "\n"))
}

// stateDetails are the state's non-empty group and category, and its completion flags, each after ", ".
func stateDetails(o textfmt.Object) string {
	var b strings.Builder
	for _, key := range []string{"group", "category"} {
		if v, ok := o.Get(key).(string); ok && v != "" {
			b.WriteString(", " + key + "=" + v)
		}
	}
	for _, key := range []string{"is_completed", "completed", "is_final"} {
		if v, ok := o.Get(key).(bool); ok {
			b.WriteString(", " + key + "=" + textfmt.Str(v))
		}
	}
	return b.String()
}

func formatWorkItems(p Page) string {
	if len(p.Results) == 0 {
		return "No Plane issues found."
	}
	lines := []string{"Plane issues:"}
	for i, v := range p.Results {
		o, _ := v.(textfmt.Object)
		lines = append(lines, fmt.Sprintf("[%d] %s%s (id=%s, state=%s, assignees=%s, priority=%s)",
			i+1, key(o), field(o, "name"), field(o, "id"), stateName(o), assignees(o), priority(o)))
	}
	return strings.Join(append(lines, listNote(p, len(p.Results))...), "\n")
}

func formatComments(p Page, item string) string {
	if len(p.Results) == 0 {
		return "No Plane comments for work_item_id=" + item + "."
	}
	lines := []string{"Plane comments for work_item_id=" + item + "\n"}
	for i, v := range p.Results {
		o, ok := v.(textfmt.Object)
		if !ok {
			lines = append(lines, fmt.Sprintf("[%d] %s", i+1, textfmt.Str(v)), "")
			continue
		}
		created := ""
		if v := firstTruthy(o.Get("created_at"), o.Get("created_on"), ""); textfmt.Truthy(v) {
			created = textfmt.Clip(strings.ReplaceAll(textfmt.Str(v), "T", " "), 16)
		}
		body := commentText(firstTruthy(o.Get("comment_html"), o.Get("comment"), ""))
		lines = append(lines, fmt.Sprintf("[%d] %s | %s", i+1, created, actor(o)), body, "")
	}
	return textfmt.Strip(strings.Join(append(lines, listNote(p, len(p.Results))...), "\n"))
}

// actor names who wrote a comment.
func actor(o textfmt.Object) string {
	switch a := firstTruthy(o.Get("actor_detail"), o.Get("actor"), o.Get("created_by"), nil).(type) {
	case textfmt.Object:
		return textfmt.Str(firstTruthy(a.Get("display_name"), a.Get("first_name"), a.Get("id"), "?"))
	case string:
		return a
	}
	return "?"
}

// formatCycleWorkItems prints the first limit rows; total is the server's count of the cycle's rows.
func formatCycleWorkItems(rows []any, total any, project, cycle string, limit int) string {
	if len(rows) == 0 {
		return fmt.Sprintf("No Plane cycle issues found for project_id=%s, cycle_id=%s.", project, cycle)
	}
	shown := clip(rows, limit)
	lines := []string{
		fmt.Sprintf("Plane cycle issues for project_id=%s, cycle_id=%s:", project, cycle),
		fmt.Sprintf("Total fetched cycle items: %d", len(rows)),
		"",
		fmt.Sprintf("Showing first %d detailed rows:", len(shown)),
	}
	for i, row := range shown {
		r, isObject := row.(textfmt.Object)
		item := row
		if isObject {
			item = firstTruthy(r.Get("work_item"), r.Get("issue_detail"), r.Get("issue"), row)
		}
		if o, ok := item.(textfmt.Object); ok {
			lines = append(lines, fmt.Sprintf("[%d] %s%s (id=%s, state=%s, assignees=%s, priority=%s, start=%s, target=%s)",
				i+1, key(o), field(o, "name"), textfmt.Str(firstTruthy(o.Get("id"), r.Get("issue"))), stateName(o),
				assignees(o), priority(o), textfmt.Str(firstTruthy(o.Get("start_date"), "?")),
				textfmt.Str(firstTruthy(o.Get("target_date"), "?"))))
		} else {
			lines = append(lines, fmt.Sprintf("[%d] work_item_id=%s", i+1, textfmt.Str(item)))
		}
		if w := r.Get("warning"); textfmt.Truthy(w) {
			lines = append(lines, "Warning: "+textfmt.Str(w))
		}
	}
	return strings.Join(append(lines, listNote(Page{TotalResults: total}, len(rows))...), "\n")
}

func formatWorkItem(v any) string {
	o, _ := v.(textfmt.Object)
	lines := []string{
		"Plane work item: " + key(o) + field(o, "name"),
		"  id: " + field(o, "id"),
		"  state: " + stateName(o),
		"  assignees: " + assignees(o),
		"  priority: " + priority(o),
	}
	description := textfmt.Str(firstTruthy(o.Get("description_html"), o.Get("description"), ""))
	description = textfmt.Strip(strings.ReplaceAll(strings.ReplaceAll(description, "<p>", ""), "</p>", "\n"))
	if description != "" {
		lines = append(lines, "  description: "+textfmt.Clip(description, 2000))
	}
	return strings.Join(lines, "\n")
}

// listNote is the line that follows shown items of a listing: how many it holds when the server's total is
// larger, else whether more may follow.
func listNote(p Page, shown int) []string {
	switch {
	case exceeds(p.TotalResults, shown):
		return []string{fmt.Sprintf("\nShowing %d of %s results.", shown, textfmt.Str(p.TotalResults))}
	case textfmt.Truthy(p.NextPageResults):
		return []string{fmt.Sprintf("\nShowing %d results. More results may exist; narrow the query if needed.", shown)}
	}
	return nil
}

// exceeds reports whether total is a number larger than n.
func exceeds(total any, n int) bool {
	switch t := total.(type) {
	case int64:
		return t > int64(n)
	case float64:
		return t > float64(n)
	}
	return false
}

// key is the work item's identifier, or else its sequence number, and a space, or "" when it has neither.
func key(o textfmt.Object) string {
	if k := firstTruthy(o.Get("identifier"), o.Get("sequence_id"), nil); k != nil {
		return textfmt.Str(k) + " "
	}
	return ""
}

// stateName is the name of the work item's state, or else the state's ID.
func stateName(o textfmt.Object) string {
	details, _ := firstTruthy(o.Get("state_details"), o.Get("state_detail"), nil).(textfmt.Object)
	return textfmt.Str(firstTruthy(details.Get("name"), o.Get("state"), "?"))
}

// assignees names the work item's assignees, or is "none"; a text in place of the list names each character.
func assignees(o textfmt.Object) string {
	v := firstTruthy(o.Get("assignee_details"), o.Get("assignees"), nil)
	list, _ := v.([]any)
	if s, ok := v.(string); ok {
		for _, r := range s {
			list = append(list, string(r))
		}
	}
	if len(list) == 0 {
		return "none"
	}
	names := make([]string, len(list))
	_, users := list[0].(textfmt.Object)
	for i, a := range list {
		if !users {
			names[i] = textfmt.Str(a)
			continue
		}
		u, _ := a.(textfmt.Object)
		names[i] = textfmt.Str(firstTruthy(u.Get("display_name"), u.Get("first_name"), get(u, "id", "?")))
	}
	joined := strings.Join(names, ", ")
	if users {
		return cmp.Or(joined, "none")
	}
	return joined
}

func priority(o textfmt.Object) string { return textfmt.Str(firstTruthy(o.Get("priority"), "none")) }

// field is the member key of o as printed, or "?" when o has none.
func field(o textfmt.Object, key string) string { return textfmt.Str(get(o, key, "?")) }

// firstTruthy is the first truthy value, or the last value when none is.
func firstTruthy(values ...any) any {
	for _, v := range values[:len(values)-1] {
		if textfmt.Truthy(v) {
			return v
		}
	}
	return values[len(values)-1]
}

// Patterns of the comment markup that becomes text; \s and \w are read as in Unicode, and an i also matches ı and
// İ.
var (
	lineBreak  = pattern(`(?i)<\s*br\s*/?\s*>`)
	blockEnd   = pattern(`(?i)<\s*/\s*(?:p|d[iıİ]v|l[iıİ]|ul|ol|blockquote)\s*>`)
	listItem   = pattern(`(?i)<\s*l[iıİ](?:[^\w>][^>]*)?>`)
	tag        = regexp.MustCompile(`<[^>]+>`)
	lineEnd    = regexp.MustCompile(`[ \t]+\n`)
	blankLines = regexp.MustCompile(`\n{3,}`)
	charRef    = regexp.MustCompile(`&(?:#[0-9]+;?|#[xX][0-9a-fA-F]+;?|[^\t\n\f <&#;]{1,32};?)`)
)

func pattern(p string) *regexp.Regexp {
	t, err := textfmt.TranslatePattern(p)
	if err != nil {
		panic(err)
	}
	return regexp.MustCompile(t)
}

// commentText is a comment's markup as text: references resolved, line breaks, block ends, and list items kept as
// line breaks and bullets, other tags removed, and blank runs collapsed.
func commentText(v any) string {
	if !textfmt.Truthy(v) {
		return ""
	}
	text := charRef.ReplaceAllStringFunc(textfmt.Str(v), unescape)
	text = lineBreak.ReplaceAllString(text, "\n")
	text = blockEnd.ReplaceAllString(text, "\n")
	text = listItem.ReplaceAllString(text, "* ")
	text = tag.ReplaceAllString(text, "")
	text = lineEnd.ReplaceAllString(text, "\n")
	return textfmt.Strip(blankLines.ReplaceAllString(text, "\n\n"))
}

// unescape resolves one character reference. A numeric one that names a control character other than tab, line
// feed, form feed, or carriage return, or a noncharacter, is dropped; NUL, a surrogate, or one beyond Unicode
// becomes U+FFFD.
func unescape(ref string) string {
	switch {
	case ref == "&nLt;": // the standard library leaves the two references longer as text than as markup
		return "\u226a\u20d2"
	case ref == "&nGt;":
		return "\u226b\u20d2"
	case ref[1] != '#':
		return html.UnescapeString(ref)
	}
	digits, base := strings.TrimSuffix(ref[2:], ";"), 10
	if digits[0] == 'x' || digits[0] == 'X' {
		digits, base = digits[1:], 16
	}
	n, err := strconv.ParseInt(digits, base, 32)
	switch {
	case n == 0 && err == nil || 0x80 <= n && n <= 0x9f:
		return html.UnescapeString("&#" + strconv.FormatInt(n, 10) + ";") // windows-1252 replacements
	case err != nil || 0xd800 <= n && n <= 0xdfff || n > 0x10ffff:
		return "\ufffd"
	case 0x1 <= n && n <= 0x8 || n == 0xb || 0xe <= n && n <= 0x1f || n == 0x7f || 0xfdd0 <= n && n <= 0xfdef || n&0xfffe == 0xfffe:
		return ""
	}
	return string(rune(n))
}
