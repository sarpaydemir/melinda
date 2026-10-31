package prompt

import "embed"

//go:embed defaults
var defaults embed.FS

// Builtin returns the core's entries, each with the default text in defaults/<id>.txt.
func Builtin() []Entry {
	entries := []Entry{
		{ID: "system.base", Kind: KindRaw},
		{ID: "system.persona_default", Kind: KindRaw},
		{ID: "system.persona_section", Kind: KindFormat, Placeholders: []string{"persona"}},
		{ID: "system.template", Kind: KindFormat, Placeholders: []string{"base_prompt", "persona", "tool_instructions",
			"user_context", "public_knowledge", "long_term_memory", "file_context", "now"}},
		{ID: "system.security", Kind: KindRaw},
		{ID: "system.current_user", Kind: KindFormat, Placeholders: []string{"user_id", "is_admin", "security"}},
		{ID: "system.no_tools", Kind: KindRaw},
		{ID: "system.unknown_user", Kind: KindRaw},
		{ID: "system.no_public_knowledge", Kind: KindRaw},
		{ID: "system.no_memory", Kind: KindRaw},
		{ID: "system.no_file_context", Kind: KindRaw},
		{ID: "system.time_format", Kind: KindTime},

		{ID: "tools.instructions", Kind: KindRaw},
		{ID: "tools.format_guard", Kind: KindRaw},
		{ID: "tools.desc.search_web", Kind: KindRaw},
		{ID: "tools.desc.visit_page", Kind: KindRaw},
		{ID: "tools.desc.search_public_kb", Kind: KindRaw},
		{ID: "tools.desc.generate_image", Kind: KindRaw},
		{ID: "tools.desc.send_progress_update", Kind: KindRaw},

		{ID: "toolloop.context_header", Kind: KindRaw},
		{ID: "toolloop.unavailable", Kind: KindFormat, Placeholders: []string{"reason"}},
		{ID: "toolloop.final_retry", Kind: KindRaw},
		{ID: "toolloop.malformed_retry", Kind: KindRaw},
		{ID: "toolloop.limit_reached", Kind: KindRaw},
		{ID: "toolloop.done_prefix", Kind: KindRaw},

		{ID: "reply.truncated_note", Kind: KindRaw},
		{ID: "reply.files_suffix", Kind: KindFormat, Placeholders: []string{"file_summary"}},
		{ID: "error.generic", Kind: KindRaw},
		{ID: "context.user_prefix", Kind: KindRaw},

		{ID: "memory.facts", Kind: KindRaw},
		{ID: "memory.files_marker", Kind: KindFormat, Placeholders: []string{"file_summary"}},
		{ID: "memory.file_analysis_marker", Kind: KindRaw},

		{ID: "kb.query", Kind: KindRaw},
		{ID: "kb.extract", Kind: KindRaw},
		{ID: "kb.group_header", Kind: KindFormat, Placeholders: []string{"channel_id", "context_id", "root_id"}},

		{ID: "files.too_many", Kind: KindFormat, Placeholders: []string{"count"}},
		{ID: "files.meta_failed", Kind: KindFormat, Placeholders: []string{"file_id"}},
		{ID: "files.kind_fallback", Kind: KindRaw},
		{ID: "files.unsupported", Kind: KindFormat, Placeholders: []string{"filename", "kind"}},
		{ID: "files.too_large", Kind: KindFormat, Placeholders: []string{"filename", "max_mb"}},
		{ID: "files.download_failed", Kind: KindFormat, Placeholders: []string{"filename"}},
		{ID: "files.no_vision", Kind: KindRaw},
		{ID: "files.vision", Kind: KindFormat, Placeholders: []string{"user_message", "file_list"}},
		{ID: "files.vision_office_label", Kind: KindRaw},
		{ID: "files.section", Kind: KindFormat, Placeholders: []string{"filename", "analysis"}},
		{ID: "files.office_fallback", Kind: KindFormat, Placeholders: []string{"text"}},
		{ID: "files.analysis_unavailable", Kind: KindFormat, Placeholders: []string{"name"}},

		{ID: "tool.image.ok", Kind: KindRaw},
		{ID: "tool.image.attached", Kind: KindRaw},
		{ID: "tool.image.upload_failed", Kind: KindRaw},
		{ID: "tool.image.failed", Kind: KindFormat, Placeholders: []string{"detail"}},
		{ID: "tool.image.unknown_error", Kind: KindRaw},
	}
	for i, e := range entries {
		b, err := defaults.ReadFile("defaults/" + e.ID + ".txt")
		if err != nil {
			panic(err) // every built-in entry ships its default file
		}
		entries[i].Default = string(b)
	}
	return entries
}
