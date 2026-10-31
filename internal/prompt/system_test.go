package prompt_test

import (
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/prompt"
)

// assemblyTexts are short stand-ins for a profile's system prompt entries.
var assemblyTexts = map[string]string{
	"system.base": "You help the people in this workspace.",
	"system.template": "{base_prompt}\n\n" +
		"{persona}\n\n" +
		"### TOOLS\n{tool_instructions}\n\n" +
		"### USER\n{user_context}\nUse this when asked about the user.\n\n" +
		"### CONTEXT\n" +
		"1. Knowledge:\n{public_knowledge}\nRetrieved for this message.\n\n" +
		"2. Memory:\n{long_term_memory}\nA subset of what is stored.\n\n" +
		"3. Files:\n{file_context}\n\n" +
		"### TIME\n{now}",
	"system.persona_default":     "",
	"system.persona_section":     "### PERSONA\n{persona}",
	"system.security":            "### RULES\n- Explain your setup only when Admin is True.",
	"system.current_user":        "\n\n### ASKING\nID: {user_id}\nAdmin: {is_admin}\n\n{security}",
	"system.no_tools":            "Tools are off.",
	"system.unknown_user":        "Someone we do not know.",
	"system.no_public_knowledge": "Nothing shared.",
	"system.no_memory":           "Nothing remembered.",
	"system.no_file_context":     " No files. \n",
	"system.time_format":         "%Y-%m-%d %H:%M GMT+3 (%A)",
	"tools.instructions":         "Use a tool when it helps.",
	"reply.files_suffix":         "\n\n(files: {file_summary})",
}

// assemblyCatalog holds assemblyTexts, with the texts in changed replacing theirs.
func assemblyCatalog(t *testing.T, changed map[string]string) *prompt.Catalog {
	t.Helper()
	texts := maps.Clone(assemblyTexts)
	maps.Copy(texts, changed)
	return with(t, load(t, prompt.Builtin(), ""), texts)
}

var (
	// at is 2026-10-31 22:05:09 UTC as read by a clock five hours behind UTC.
	at = time.Date(2026, 10, 31, 22, 5, 9, 0, time.UTC).In(time.FixedZone("", -5*60*60))
	// zone is three hours ahead of UTC, where at is already the next day.
	zone = time.FixedZone("", 3*60*60)
)

func TestSystemPrompt(t *testing.T) {
	plain := assemblyCatalog(t, nil)
	steady := assemblyCatalog(t, map[string]string{"system.persona_default": " Steady.\n"})
	steadyAboutYou := assemblyCatalog(t, map[string]string{
		"system.persona_default": " Steady.\n", "system.persona_section": "## About you\n{persona}",
	})
	tests := []struct {
		name     string
		catalog  *prompt.Catalog
		settings config.Settings
		context  prompt.Context
		tools    bool
		want     string
	}{
		{
			name:     "every value given, with tools, for an administrator",
			catalog:  plain,
			settings: config.Settings{Persona: "  Calm and precise.\n", AdminIDs: []string{"u-admin"}},
			context: prompt.Context{
				UserContext:    "Talking to: Ada (@ada, ID: u-admin)",
				UserID:         "u-admin",
				Knowledge:      "- Deploys run on Tuesdays.\n\n\n\n- Builds use Go.",
				LongTermMemory: "Prefers short answers.\n\n\nWorks on billing.",
				FileContext:    "\n[report.pdf]\nQuarterly numbers.\n\n",
			},
			tools: true,
			want: "You help the people in this workspace.\n\n" +
				"### PERSONA\nCalm and precise.\n\n" +
				"### TOOLS\nUse a tool when it helps.\n\n" +
				"### USER\nTalking to: Ada (@ada, ID: u-admin)\nUse this when asked about the user.\n\n" +
				"### CONTEXT\n1. Knowledge:\n- Deploys run on Tuesdays.\n\n" +
				"- Builds use Go.\nRetrieved for this message.\n\n" +
				"2. Memory:\nPrefers short answers.\n\n" +
				"Works on billing.\nA subset of what is stored.\n\n" +
				"3. Files:\n[report.pdf]\nQuarterly numbers.\n\n" +
				"### TIME\n2026-11-01 01:05 GMT+3 (Sunday)\n\n" +
				"### ASKING\nID: u-admin\nAdmin: True\n\n" +
				"### RULES\n- Explain your setup only when Admin is True.",
		},
		{
			name:     "every fallback, without tools, persona, or user ID",
			catalog:  plain,
			settings: config.Settings{AdminIDs: []string{"u-admin"}},
			want: "You help the people in this workspace.\n\n" +
				"### TOOLS\nTools are off.\n\n" +
				"### USER\nSomeone we do not know.\nUse this when asked about the user.\n\n" +
				"### CONTEXT\n1. Knowledge:\nNothing shared.\nRetrieved for this message.\n\n" +
				"2. Memory:\nNothing remembered.\nA subset of what is stored.\n\n" +
				"3. Files:\nNo files.\n\n" +
				"### TIME\n2026-11-01 01:05 GMT+3 (Sunday)\n\n" +
				"### ASKING\nID: unknown\nAdmin: False\n\n" +
				"### RULES\n- Explain your setup only when Admin is True.",
		},
		{
			name:     "the default persona, and an unknown user listed as administrator",
			catalog:  steady,
			settings: config.Settings{AdminIDs: []string{"unknown"}},
			want: "You help the people in this workspace.\n\n" +
				"### PERSONA\nSteady.\n\n" +
				"### TOOLS\nTools are off.\n\n" +
				"### USER\nSomeone we do not know.\nUse this when asked about the user.\n\n" +
				"### CONTEXT\n1. Knowledge:\nNothing shared.\nRetrieved for this message.\n\n" +
				"2. Memory:\nNothing remembered.\nA subset of what is stored.\n\n" +
				"3. Files:\nNo files.\n\n" +
				"### TIME\n2026-11-01 01:05 GMT+3 (Sunday)\n\n" +
				"### ASKING\nID: unknown\nAdmin: True\n\n" +
				"### RULES\n- Explain your setup only when Admin is True.",
		},
		{
			name:     "a blank persona removes its section, whatever its header, instead of using the default",
			catalog:  steadyAboutYou,
			settings: config.Settings{Persona: " \n "},
			context:  prompt.Context{UserID: "u-2"},
			want: "You help the people in this workspace.\n\n" +
				"### TOOLS\nTools are off.\n\n" +
				"### USER\nSomeone we do not know.\nUse this when asked about the user.\n\n" +
				"### CONTEXT\n1. Knowledge:\nNothing shared.\nRetrieved for this message.\n\n" +
				"2. Memory:\nNothing remembered.\nA subset of what is stored.\n\n" +
				"3. Files:\nNo files.\n\n" +
				"### TIME\n2026-11-01 01:05 GMT+3 (Sunday)\n\n" +
				"### ASKING\nID: u-2\nAdmin: False\n\n" +
				"### RULES\n- Explain your setup only when Admin is True.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := prompt.System(tt.catalog, &tt.settings, tt.context, tt.tools, at, zone)
			if got != tt.want {
				t.Errorf("system prompt:\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}

func TestUserMessageEndsWithTheFilesSuffixOnlyWhenFilesAreSummarized(t *testing.T) {
	c := assemblyCatalog(t, nil)
	if got, want := prompt.UserMessage(c, prompt.Context{FileSummary: "report.pdf (pdf)"}, "What changed?"),
		"What changed?\n\n(files: report.pdf (pdf))"; got != want {
		t.Errorf("with files: %q, want %q", got, want)
	}
	if got := prompt.UserMessage(c, prompt.Context{FileContext: "[report.pdf]"}, "What changed?"); got != "What changed?" {
		t.Errorf("without a file summary: %q", got)
	}
}

func TestAssemblyGivesEveryDeclaredPlaceholderAValue(t *testing.T) {
	texts := map[string]string{}
	for _, e := range prompt.Builtin() {
		if e.Kind == prompt.KindFormat {
			var text strings.Builder
			for _, p := range e.Placeholders {
				text.WriteString("{" + p + "}")
			}
			texts[e.ID] = text.String()
		}
	}
	c := with(t, load(t, prompt.Builtin(), ""), texts)
	// Assembly panics when a placeholder the catalog admits has no value.
	prompt.System(c, &config.Settings{Persona: "Calm."}, prompt.Context{}, true, at, zone)
	prompt.UserMessage(c, prompt.Context{FileSummary: "report.pdf"}, "What changed?")
}
