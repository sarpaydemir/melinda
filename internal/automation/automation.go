package automation

import (
	"context"
	"regexp"

	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

// Handler runs a matched automation, posting its own messages, and returns the reply saved as the turn. A reply
// holding a failure code is not saved.
type Handler func(ctx context.Context, r Run) (string, error)

// Run is what a handler works with while answering one message.
type Run struct {
	Message  chat.Message
	Settings *config.Settings
	// Args are the handler arguments, each its capture group stripped.
	Args map[string]string
	// Chat is the run's outbound path to the chat platform.
	Chat interface {
		Send(ctx context.Context, m chat.Outgoing) (string, error)
	}
}

// mention is an @mention and the whitespace after it.
var mention = regexp.MustCompile(func() string {
	p, err := textfmt.TranslatePattern(`@\w+\s*`)
	if err != nil {
		panic(err)
	}
	return p
}())

// Match returns the first of automations whose pattern occurs in text with every mention removed and the result
// stripped, and the handler arguments read from that match.
func Match(automations []config.Automation, text string) (config.Automation, map[string]string, bool) {
	text = textfmt.Strip(mention.ReplaceAllString(text, ""))
	for _, a := range automations {
		groups := a.Regexp.FindStringSubmatch(text)
		if groups == nil {
			continue
		}
		args := make(map[string]string, len(a.Args))
		for arg, g := range a.Args {
			args[arg] = textfmt.Strip(groups[g])
		}
		return a, args, true
	}
	return config.Automation{}, nil, false
}
