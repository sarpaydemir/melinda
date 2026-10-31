package automation_test

import (
	"encoding/json"
	"maps"
	"testing"

	"github.com/sarpaydemir/dost/internal/automation"
	"github.com/sarpaydemir/dost/internal/config"
)

func TestFirstDeclaredAutomationMatchingTheTextWithoutMentionsWins(t *testing.T) {
	doc := map[string]json.RawMessage{"model": json.RawMessage(`"m"`), "automations": json.RawMessage(`[
		{"name": "brand", "pattern": "/marka\\s+(.+)", "handler": "h", "args": {"brand": 1}},
		{"name": "report", "pattern": "(\\w+)\\s*/rapor", "handler": "h", "args": {"topic": 1}},
		{"name": "learn", "pattern": "/öğren\\s+(.+)", "handler": "h", "args": {"x": 1}}
	]`)}
	s, err := config.Load(doc, "agent", nil, func(*config.Settings) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		text, name string
		args       map[string]string
	}{
		{"@çağrı_2 /marka Acme", "brand", map[string]string{"brand": "Acme"}},
		{"@İsmail\u00a0\u2003\x1c/marka Acme", "brand", map[string]string{"brand": "Acme"}},
		{"@bot\t\n /marka Acme", "brand", map[string]string{"brand": "Acme"}},
		{"mail x@y.com /MARKA Acme", "brand", map[string]string{"brand": "Acme"}},
		{"konu /rapor ve /marka Acme", "brand", map[string]string{"brand": "Acme"}},
		{"merhaba, şunu yap: /marka   Acme Corp \nteşekkürler", "brand", map[string]string{"brand": "Acme Corp"}},
		{"@yardımcı özet\u3000 /RAPOR", "report", map[string]string{"topic": "özet"}},
		{"/ÖĞREN Şirket", "learn", map[string]string{"x": "Şirket"}},
		{"/marka@bot Acme", "", nil},
		{"@a @b", "", nil},
		{"hello there", "", nil},
	} {
		a, args, ok := automation.Match(s.Automations, tc.text)
		if ok != (tc.name != "") || a.Name != tc.name || !maps.Equal(args, tc.args) {
			t.Errorf("Match(%q) = %q %q %v, want %q %q", tc.text, a.Name, args, ok, tc.name, tc.args)
		}
	}
}
