// Package brand is the brand analysis automation: it researches a brand on the web, analyses its positioning, and
// drafts slogans and video concepts, posting each step as it completes.
package brand

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/sarpaydemir/dost/internal/automation"
	"github.com/sarpaydemir/dost/internal/clock"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/port/model"
	"github.com/sarpaydemir/dost/internal/prompt"
	"github.com/sarpaydemir/dost/internal/routing"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

const (
	searchLimit  = 3
	contentLimit = 5000 // code points of the page text the web prompt gets
	learnLimit   = 4000 // code points of a step message queued for learning
	// attempts is how often each phase runs before the analysis fails, retryDelay the wait between attempts.
	attempts   = 3
	retryDelay = 10 * time.Second
)

//go:embed defaults
var defaults embed.FS

// Prompts returns the catalog entries of the automation's texts.
func Prompts() []prompt.Entry {
	format := func(id string, placeholders ...string) prompt.Entry {
		return prompt.Entry{ID: id, Kind: prompt.KindFormat, Placeholders: placeholders}
	}
	raw := func(id string) prompt.Entry { return prompt.Entry{ID: id, Kind: prompt.KindRaw} }
	entries := []prompt.Entry{
		raw("prefix"), raw("suffix"),
		format("web", "brand", "url", "content"),
		format("instagram", "brand", "handle"),
		format("analysis", "brand", "web_summary", "products", "instagram_summary", "themes"),
		format("slogans", "brand", "categories", "target", "web_summary"),
		format("videos", "brand", "slogans", "target"),
		raw("empty"), raw("empty_reply"),
		format("start", "brand"),
		format("step", "step", "title", "message"),
		format("step_memory", "brand", "step"),
		format("done", "brand"),
		format("summary", "brand", "categories", "target", "price", "slogans", "videos"),
		format("error", "error"), format("error_reply", "error"),
		format("web_message", "url", "summary"),
		format("instagram_message", "handle", "summary"),
		format("analysis_message", "categories", "target", "price"),
		raw("title_web"), raw("title_instagram"), raw("title_analysis"), raw("title_slogans"), raw("title_videos"),
		raw("default_web_summary"), raw("default_no_website"), raw("default_instagram_summary"),
		raw("default_no_instagram"), raw("default_no_instagram_step"), raw("default_category"), raw("default_target"),
		raw("default_price"), format("default_slogan_text", "brand"), raw("default_slogan_why"),
		format("default_video_title", "brand"), raw("default_video_idea"), raw("default_video_duration"),
	}
	for i := range entries {
		entries[i].ID = "automation.brand." + entries[i].ID
		b, err := defaults.ReadFile("defaults/" + entries[i].ID + ".txt")
		if err != nil {
			panic(err) // every entry ships its default file
		}
		entries[i].Default = string(b)
	}
	return entries
}

// Analyze analyses the brand named by the brand_name argument. It posts a start message, one message per step, and
// a completion message, and returns the full summary without posting it. Each step is also added to the
// conversation histories and queued for learning. A failed phase runs again from its first step not yet posted;
// when it keeps failing, the error is posted and the error reply returned. A run taken over stops at once with
// chat.ErrTakenOver.
func Analyze(ctx context.Context, r automation.Run) (string, error) {
	a := &analysis{Run: r, brand: r.Args["brand_name"], root: routing.ReplyThread(r.Message)}
	if a.brand == "" {
		if err := a.post(ctx, a.text("empty")); err != nil {
			return "", err
		}
		return a.text("empty_reply"), nil
	}
	if err := a.post(ctx, a.render("start", "brand", a.brand)); err != nil {
		return "", err
	}
	ctx = context.WithoutCancel(ctx) // posting has started
	steps := []func(context.Context) error{a.website, a.instagram, a.analyse, a.slogans, a.videos}
	for _, end := range []int{2, len(steps)} {
		if err := a.phase(ctx, steps, end); errors.Is(err, chat.ErrTakenOver) {
			return "", err
		} else if err != nil {
			r.Log.ErrorContext(ctx, "brand analysis failed", "brand", a.brand, "error", err)
			if err := a.post(ctx, a.render("error", "error", err.Error())); err != nil {
				return "", err
			}
			return a.render("error_reply", "error", err.Error()), nil
		}
	}
	slogans, videos := make([]string, len(a.sloganList)), make([]string, len(a.videoList))
	for i, s := range a.sloganList {
		slogans[i] = "• " + textfmt.Str(get(s, "text", "")) + " - " + textfmt.Str(get(s, "why", ""))
	}
	for i, v := range a.videoList {
		videos[i] = "• " + textfmt.Str(get(v, "title", "")) + " (" + textfmt.Str(get(v, "duration", "?")) + ") - " +
			textfmt.Str(get(v, "idea", ""))
	}
	summary := a.render("summary", "brand", a.brand, "categories", a.categoryText, "target", textfmt.Str(a.target),
		"price", textfmt.Str(a.price), "slogans", strings.Join(slogans, "\n"), "videos", strings.Join(videos, "\n"))
	if err := a.post(ctx, a.render("done", "brand", a.brand)); err != nil {
		return "", err
	}
	return summary, nil
}

// analysis is one run's progress; it survives a phase's retries.
type analysis struct {
	automation.Run
	brand, root string
	// posted counts the steps whose message was posted; those steps never run again.
	posted int

	webSummary, products  any
	instaSummary, themes  any
	categories            any
	categoryText          string // the categories joined with ", "
	target, price         any
	sloganList, videoList []textfmt.Object
}

// phase runs the steps not yet posted up to end, and runs them again after a failure, at most attempts times.
func (a *analysis) phase(ctx context.Context, steps []func(context.Context) error, end int) error {
	var err error
	for attempt := 1; ; attempt++ {
		for err = nil; err == nil && a.posted < end; {
			err = steps[a.posted](ctx)
		}
		if err == nil || attempt == attempts || errors.Is(err, chat.ErrTakenOver) {
			return err
		}
		a.Log.WarnContext(ctx, "brand analysis step failed; retrying", "brand", a.brand, "step", a.posted+1, "error", err)
		_ = clock.Sleep(ctx, a.Clock, retryDelay)
	}
}

func (a *analysis) website(ctx context.Context) error {
	results, err := a.Search.Search(ctx, a.brand+" official website", searchLimit)
	if err != nil {
		a.Log.ErrorContext(ctx, "web search failed", "error", err)
	}
	title := a.text("title_web")
	if len(results) == 0 {
		a.webSummary, a.products = a.text("default_no_website"), []any{}
		return a.notify(ctx, title, a.text("default_no_website"))
	}
	if results[0].URL == nil {
		return errors.New("'NoneType' object has no attribute 'startswith'")
	}
	url := *results[0].URL
	content, err := a.Fetch.Fetch(ctx, url)
	if err != nil {
		content = err.Error()
	}
	data, err := a.ask(ctx, a.render("web", "brand", a.brand, "url", url, "content", textfmt.Clip(content, contentLimit)))
	if err != nil {
		return err
	}
	a.webSummary, a.products = get(data, "summary", a.text("default_web_summary")), get(data, "products", []any{})
	return a.notify(ctx, title, a.render("web_message", "url", url, "summary", textfmt.Str(a.webSummary)))
}

func (a *analysis) instagram(ctx context.Context) error {
	results, err := a.Search.Search(ctx, a.brand+" instagram", searchLimit)
	if err != nil {
		a.Log.ErrorContext(ctx, "web search failed", "error", err)
	}
	var profile *string
	for _, r := range results {
		if r.URL == nil {
			return errors.New("argument of type 'NoneType' is not iterable")
		}
		if profile == nil && strings.Contains(*r.URL, "instagram.com") {
			profile = r.URL
		}
	}
	title := a.text("title_instagram")
	if profile == nil {
		a.instaSummary, a.themes = a.text("default_no_instagram"), []any{}
		return a.notify(ctx, title, a.text("default_no_instagram_step"))
	}
	parts := strings.Split(*profile, "instagram.com/")
	handle := "@" + strings.Split(strings.Trim(parts[len(parts)-1], "/"), "/")[0]
	data, err := a.ask(ctx, a.render("instagram", "brand", a.brand, "handle", handle))
	if err != nil {
		return err
	}
	a.instaSummary, a.themes = get(data, "summary", a.text("default_instagram_summary")), get(data, "themes", []any{})
	return a.notify(ctx, title, a.render("instagram_message", "handle", handle, "summary", textfmt.Str(a.instaSummary)))
}

func (a *analysis) analyse(ctx context.Context) error {
	data, err := a.ask(ctx, a.render("analysis", "brand", a.brand, "web_summary", textfmt.Str(a.webSummary),
		"products", textfmt.Str(a.products), "instagram_summary", textfmt.Str(a.instaSummary),
		"themes", textfmt.Str(a.themes)))
	if err != nil {
		return err
	}
	categories := get(data, "categories", []any{a.text("default_category")})
	joined, err := join(categories)
	if err != nil {
		return err
	}
	a.categories, a.categoryText = categories, joined
	a.target, a.price = get(data, "target", a.text("default_target")), get(data, "price", a.text("default_price"))
	return a.notify(ctx, a.text("title_analysis"), a.render("analysis_message", "categories", joined,
		"target", textfmt.Str(a.target), "price", textfmt.Str(a.price)))
}

func (a *analysis) slogans(ctx context.Context) error {
	data, err := a.ask(ctx, a.render("slogans", "brand", a.brand, "categories", textfmt.Str(a.categories),
		"target", textfmt.Str(a.target), "web_summary", textfmt.Str(a.webSummary)))
	if err != nil {
		return err
	}
	list, err := records(get(data, "slogans", []any{textfmt.Object{
		{Key: "text", Value: a.render("default_slogan_text", "brand", a.brand)},
		{Key: "why", Value: a.text("default_slogan_why")},
	}}))
	if err != nil {
		return err
	}
	lines := make([]string, len(list))
	for i, s := range list {
		lines[i] = "• **" + textfmt.Str(get(s, "text", "")) + "** - " + textfmt.Str(get(s, "why", ""))
	}
	a.sloganList = list
	return a.notify(ctx, a.text("title_slogans"), strings.Join(lines, "\n"))
}

func (a *analysis) videos(ctx context.Context) error {
	texts := make([]any, len(a.sloganList))
	for i, s := range a.sloganList {
		texts[i] = get(s, "text", nil)
	}
	data, err := a.ask(ctx, a.render("videos", "brand", a.brand, "slogans", textfmt.Repr(texts),
		"target", textfmt.Str(a.target)))
	if err != nil {
		return err
	}
	list, err := records(get(data, "videos", []any{textfmt.Object{
		{Key: "title", Value: a.render("default_video_title", "brand", a.brand)},
		{Key: "idea", Value: a.text("default_video_idea")},
		{Key: "duration", Value: a.text("default_video_duration")},
	}}))
	if err != nil {
		return err
	}
	lines := make([]string, len(list))
	for i, v := range list {
		lines[i] = "• **" + textfmt.Str(get(v, "title", "")) + "** (" + textfmt.Str(get(v, "duration", "")) + "): " +
			textfmt.Str(get(v, "idea", ""))
	}
	a.videoList = list
	return a.notify(ctx, a.text("title_videos"), strings.Join(lines, "\n"))
}

// ask sends prompt, framed by the prefix and suffix, as the only message and decodes the answer. An answer that is
// not JSON, or a falsy value, reads as an empty object; any other value that is not an object is an error.
func (a *analysis) ask(ctx context.Context, p string) (textfmt.Object, error) {
	text := a.text("prefix") + p + a.text("suffix")
	answer := textfmt.Strip(a.Model.Generate(ctx, model.Request{Model: a.Settings.Model,
		Messages: []model.Message{{Role: model.RoleUser, Content: text}}}))
	if strings.Contains(answer, "```") {
		answer = textfmt.Strip(strings.Replace(strings.Split(answer, "```")[1], "json", "", 1))
	}
	v, err := textfmt.Decode([]byte(answer), true)
	if err != nil || !textfmt.Truthy(v) {
		return nil, nil
	}
	obj, ok := v.(textfmt.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", typeName(v))
	}
	return obj, nil
}

// notify posts step message under title, adds it to the conversation histories, and queues it for learning. The
// step counts as posted once its post was attempted.
func (a *analysis) notify(ctx context.Context, title, message string) error {
	a.posted++
	step := a.posted
	if err := a.post(ctx, a.render("step", "step", strconv.Itoa(step), "title", title, "message", message)); err != nil {
		return err
	}
	now := a.Clock.Now()
	item, _ := json.Marshal(struct {
		User      string `json:"user"`
		Assistant string `json:"assistant"`
		TS        string `json:"ts"`
		Scope     string `json:"scope"`
		ContextID string `json:"context_id"`
		Step      int    `json:"step"`
	}{a.render("step_memory", "brand", a.brand, "step", strconv.Itoa(step)), "**" + title + ":**\n" + message,
		textfmt.FormatTime(now), "public", a.root, step})
	keys := []string{a.Message.AuthorID}
	if a.root != "" {
		keys = []string{"thread:" + a.root, a.Message.AuthorID}
	}
	for _, key := range keys {
		if err := a.History.Add(ctx, key, item); err != nil {
			return fmt.Errorf("add step to history %s: %w", key, err)
		}
	}
	queued, _ := json.Marshal(struct {
		PostID    string  `json:"post_id"`
		Message   string  `json:"message"`
		ChannelID string  `json:"channel_id"`
		CreateAt  float64 `json:"create_at"`
	}{fmt.Sprintf("brand:%s:%d:%d", a.brand, step, now.Unix()), a.brand + " - " + title + "\n" +
		textfmt.Clip(message, learnLimit), "brand_analysis", float64(now.UnixNano()) / 1e9})
	if err := a.Knowledge.AppendRaw(ctx, a.Settings, a.Message, queued); err != nil {
		a.Log.WarnContext(ctx, "brand analysis step not queued for learning", "step", step, "error", err)
	}
	return nil
}

// post sends text to the conversation, in the reply thread. A failed post is logged; only a run that was cancelled
// before it wrote anything, or taken over, stops.
func (a *analysis) post(ctx context.Context, text string) error {
	_, err := a.Chat.Send(ctx, chat.Outgoing{ConversationID: a.Message.ConversationID, ThreadID: a.root, Text: text})
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, chat.ErrTakenOver) {
			return err
		}
		a.Log.ErrorContext(ctx, "brand analysis message not posted", "error", err)
	}
	return nil
}

func (a *analysis) text(id string) string { return a.Catalog.Text("automation.brand." + id) }

// render fills the entry id with the values given as name, value pairs.
func (a *analysis) render(id string, pairs ...string) string {
	values := make(map[string]string, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		values[pairs[i]] = pairs[i+1]
	}
	return a.Catalog.Render("automation.brand."+id, values)
}

// get returns the value of the member key of o, or def when o has none.
func get(o textfmt.Object, key string, def any) any {
	for _, m := range o {
		if m.Key == key {
			return m.Value
		}
	}
	return def
}

// elements returns what iterating over v yields: a list's items, an object's keys, or a string's characters.
func elements(v any) ([]any, bool) {
	switch v := v.(type) {
	case []any:
		return v, true
	case textfmt.Object:
		keys := make([]any, len(v))
		for i, m := range v {
			keys[i] = m.Key
		}
		return keys, true
	case string:
		chars := make([]any, 0, len(v))
		for _, r := range v {
			chars = append(chars, string(r))
		}
		return chars, true
	}
	return nil, false
}

// join joins the strings v yields with ", ".
func join(v any) (string, error) {
	items, ok := elements(v)
	if !ok {
		return "", errors.New("can only join an iterable")
	}
	parts := make([]string, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			return "", fmt.Errorf("sequence item %d: expected str instance, %s found", i, typeName(item))
		}
		parts[i] = s
	}
	return strings.Join(parts, ", "), nil
}

// records returns the objects v yields.
func records(v any) ([]textfmt.Object, error) {
	items, ok := elements(v)
	if !ok {
		return nil, fmt.Errorf("'%s' object is not iterable", typeName(v))
	}
	list := make([]textfmt.Object, len(items))
	for i, item := range items {
		obj, ok := item.(textfmt.Object)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", typeName(item))
		}
		list[i] = obj
	}
	return list, nil
}

// typeName names the JSON type of v as the messages above do.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case int64, *big.Int:
		return "int"
	case float64:
		return "float"
	case []any:
		return "list"
	}
	return "dict"
}
