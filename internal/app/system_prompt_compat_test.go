package app

import (
	"cmp"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/clock"
	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/prompt"
)

// TestCompatProfileAssemblesItsSystemPrompts builds the system prompt of each fixture in the private profile's
// assembled/*.json ({"settings", "context", "tools", "now", "zone_offset_seconds", "expected"}) with the
// profile's prompts and compares it byte for byte with expected, or with accepted where the fixture records an
// accepted difference.
func TestCompatProfileAssemblesItsSystemPrompts(t *testing.T) {
	dir := os.Getenv("DOST_COMPAT_PROFILE")
	if dir == "" {
		t.Skip("DOST_COMPAT_PROFILE is not set")
	}
	cat, err := prompt.Load(builtin(http.DefaultTransport, clock.Real(), "melinda/test", "", slog.New(slog.DiscardHandler)).prompts(), filepath.Join(dir, "prompts"))
	if err != nil {
		t.Fatal(err)
	}
	fixtures, _ := filepath.Glob(filepath.Join(dir, "assembled", "*.json"))
	if len(fixtures) == 0 {
		t.Skip("the profile has no assembled fixtures")
	}
	for _, path := range fixtures {
		t.Run(filepath.Base(path), func(t *testing.T) {
			var f struct {
				Settings map[string]json.RawMessage `json:"settings"`
				Context  prompt.Context             `json:"context"`
				Tools    bool                       `json:"tools"`
				Now      time.Time                  `json:"now"`
				Offset   int                        `json:"zone_offset_seconds"`
				Expected string                     `json:"expected"`
				Accepted string                     `json:"accepted"`
			}
			b, err := os.ReadFile(path)
			if err == nil {
				err = json.Unmarshal(b, &f)
			}
			if err != nil {
				t.Fatalf("not an assembled fixture: %v", err)
			}
			if f.Settings == nil {
				f.Settings = map[string]json.RawMessage{}
			}
			f.Settings["model"] = json.RawMessage(`"m"`)
			s, err := config.Load(f.Settings, "agent", nil, func(*config.Settings) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			got := prompt.System(cat, s, f.Context, f.Tools, f.Now, time.FixedZone("", f.Offset))
			if want := cmp.Or(f.Accepted, f.Expected); got != want {
				t.Errorf("system prompt\n%q\nwant\n%q", got, want)
			}
		})
	}
}
