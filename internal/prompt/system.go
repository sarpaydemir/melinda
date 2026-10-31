package prompt

import (
	"cmp"
	"strings"
	"time"

	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

// unknownUserID stands in for a missing user ID, in the prompt and in the administrator check.
const unknownUserID = "unknown"

// System returns the system prompt for a reply in the context c, showing now in zone. tools selects
// tools.instructions over system.no_tools, and a persona fills system.persona_section. The filled system.template
// is split at blank lines, each part is stripped, empty parts are dropped, and system.current_user follows unchanged.
func System(cat *Catalog, s *config.Settings, c Context, tools bool, now time.Time, zone *time.Location) string {
	toolText := cat.Text("system.no_tools")
	if tools {
		toolText = cat.Text("tools.instructions")
	}
	persona := textfmt.Strip(cmp.Or(s.Persona, cat.Text("system.persona_default")))
	if persona != "" {
		persona = cat.render("system.persona_section", map[string]string{"persona": persona})
	}
	filled := cat.render("system.template", map[string]string{
		"base_prompt":       cat.Text("system.base"),
		"persona":           persona,
		"tool_instructions": toolText,
		"user_context":      cmp.Or(c.UserContext, cat.Text("system.unknown_user")),
		"public_knowledge":  cmp.Or(c.Knowledge, cat.Text("system.no_public_knowledge")),
		"long_term_memory":  cmp.Or(c.LongTermMemory, cat.Text("system.no_memory")),
		"file_context":      textfmt.Strip(cmp.Or(c.FileContext, cat.Text("system.no_file_context"))),
		"now":               textfmt.Strftime(now.In(zone), cat.Text("system.time_format")),
	})
	var parts []string
	for _, p := range strings.Split(filled, "\n\n") {
		if p = textfmt.Strip(p); p != "" {
			parts = append(parts, p)
		}
	}
	userID := cmp.Or(c.UserID, unknownUserID)
	return strings.Join(parts, "\n\n") + cat.render("system.current_user", map[string]string{
		"user_id":  userID,
		"is_admin": textfmt.Str(s.IsAdmin(userID)),
		"security": cat.Text("system.security"),
	})
}

// UserMessage returns message followed by reply.files_suffix when c has a file summary.
func UserMessage(cat *Catalog, c Context, message string) string {
	if c.FileSummary == "" {
		return message
	}
	return message + cat.render("reply.files_suffix", map[string]string{"file_summary": c.FileSummary})
}

// render fills the entry id with values, which must cover every placeholder the entry declares.
func (c *Catalog) render(id string, values map[string]string) string {
	s, err := c.Template(id).Render(values)
	if err != nil {
		panic(err) // the catalog admits only declared placeholders, so a miss is a programming error
	}
	return s
}
