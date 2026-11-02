package prompt

import (
	"cmp"
	"maps"
	"strings"
	"time"

	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/textfmt"
)

// unknownUserID stands in for a missing user ID, in the prompt and in the administrator check.
const unknownUserID = "unknown"

// System returns the system prompt for a reply in the context c, showing now in zone. tools selects
// tools.instructions over system.no_tools, and a persona fills system.persona_section. With context_role user,
// system.context_moved stands in for the knowledge, memory, and file context. The filled system.template is split
// at blank lines, each part is stripped, empty parts are dropped, and system.current_user follows unchanged.
func System(cat *Catalog, s *config.Settings, c Context, tools bool, now time.Time, zone *time.Location) string {
	toolText := cat.Text("system.no_tools")
	if tools {
		toolText = cat.Text("tools.instructions")
	}
	persona := textfmt.Strip(cmp.Or(s.Persona, cat.Text("system.persona_default")))
	if persona != "" {
		persona = cat.Render("system.persona_section", map[string]string{"persona": persona})
	}
	values := map[string]string{
		"base_prompt":       cat.Text("system.base"),
		"persona":           persona,
		"tool_instructions": toolText,
		"user_context":      cmp.Or(c.UserContext, cat.Text("system.unknown_user")),
		"now":               textfmt.Strftime(now.In(zone), cat.Text("system.time_format")),
	}
	if s.ContextRole == "user" {
		moved := cat.Text("system.context_moved")
		values["public_knowledge"], values["long_term_memory"], values["file_context"] = moved, moved, moved
	} else {
		maps.Copy(values, contextValues(cat, c))
	}
	var parts []string
	for _, p := range strings.Split(cat.Render("system.template", values), "\n\n") {
		if p = textfmt.Strip(p); p != "" {
			parts = append(parts, p)
		}
	}
	userID := cmp.Or(c.UserID, unknownUserID)
	return strings.Join(parts, "\n\n") + cat.Render("system.current_user", map[string]string{
		"user_id":  userID,
		"is_admin": textfmt.Str(s.IsAdmin(userID)),
		"security": cat.Text("system.security"),
	})
}

// ContextMessage returns the filled context.message, sent as a user message just before the user's message, when
// context_role is user, and "" otherwise.
func ContextMessage(cat *Catalog, s *config.Settings, c Context) string {
	if s.ContextRole != "user" {
		return ""
	}
	values := contextValues(cat, c)
	for k, v := range values {
		values[k] = contextTags.Replace(v)
	}
	return cat.Render("context.message", values)
}

// contextTags breaks the context delimiters inside a value with a zero-width space, so it cannot end the block.
var contextTags = strings.NewReplacer("<context>", "<\u200bcontext>", "</context>", "<\u200b/context>")

// contextValues returns the knowledge, memory, and file context of c, each with its fallback.
func contextValues(cat *Catalog, c Context) map[string]string {
	return map[string]string{
		"public_knowledge": cmp.Or(c.Knowledge, cat.Text("system.no_public_knowledge")),
		"long_term_memory": cmp.Or(c.LongTermMemory, cat.Text("system.no_memory")),
		"file_context":     textfmt.Strip(cmp.Or(c.FileContext, cat.Text("system.no_file_context"))),
	}
}

// UserMessage returns message followed by reply.files_suffix when c has a file summary.
func UserMessage(cat *Catalog, c Context, message string) string {
	if c.FileSummary == "" {
		return message
	}
	return message + cat.Render("reply.files_suffix", map[string]string{"file_summary": c.FileSummary})
}
