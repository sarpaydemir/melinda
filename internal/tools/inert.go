package tools

import "strings"

// inert returns the markdown text as one fenced code block, which clients and the server render literally: no image,
// link, or autolink in it is fetched. The fence is longer than any backtick run in text, so no line can close it.
func inert(text string) string {
	longest, run := 0, 0
	for i := range len(text) {
		if text[i] != '`' {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "\n" + text + "\n" + fence
}
