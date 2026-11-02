package prompt_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sarpaydemir/melinda/internal/prompt"
)

var entries = []prompt.Entry{
	{ID: "greeting", Kind: prompt.KindFormat, Placeholders: []string{"name"}, Default: "Hello, {name}."},
	{ID: "label", Kind: prompt.KindRaw, Default: "{label}"},
	{ID: "clock", Kind: prompt.KindTime, Default: "%H:%M"},
}

var ada = map[string]string{"name": "Ada"}

// profile writes files into a new directory and returns its path.
func profile(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func load(t *testing.T, entries []prompt.Entry, dir string) *prompt.Catalog {
	t.Helper()
	c, err := prompt.Load(entries, dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func with(t *testing.T, c *prompt.Catalog, overrides map[string]string) *prompt.Catalog {
	t.Helper()
	c, err := c.With(overrides)
	if err != nil {
		t.Fatalf("With: %v", err)
	}
	return c
}

func render(t *testing.T, c *prompt.Catalog, id string, values map[string]string) string {
	t.Helper()
	got, err := c.Template(id).Render(values)
	if err != nil {
		t.Fatalf("render %s: %v", id, err)
	}
	return got
}

func TestBuiltinEntriesLoadFromTheirDefaultFiles(t *testing.T) {
	builtin := prompt.Builtin()
	c := load(t, builtin, "")
	var ids []string
	for _, e := range builtin {
		ids = append(ids, e.ID)
		if c.Text(e.ID) != e.Default {
			t.Errorf("%s: text %q, want its default %q", e.ID, c.Text(e.ID), e.Default)
		}
	}
	files, err := os.ReadDir("defaults")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, strings.TrimSuffix(f.Name(), ".txt"))
	}
	slices.Sort(ids)
	if !slices.Equal(names, ids) {
		t.Errorf("default files %v, want one per entry %v", names, ids)
	}
}

func TestLaterLayersWin(t *testing.T) {
	dir := profile(t, map[string]string{"greeting.txt": "Hi {name}!", "label.txt": "profile label"})
	c := with(t, load(t, entries, dir), map[string]string{"label": "settings label"})

	got := []string{c.Text("greeting"), c.Text("label"), c.Text("clock")}
	if want := []string{"Hi {name}!", "settings label", "%H:%M"}; !slices.Equal(got, want) {
		t.Errorf("texts %q, want %q", got, want)
	}
	if got := render(t, c, "greeting", ada); got != "Hi Ada!" {
		t.Errorf("greeting renders %q", got)
	}
}

func TestEntryKindDecidesWhetherBracesArePlaceholders(t *testing.T) {
	c := with(t, load(t, entries, ""), map[string]string{
		"greeting": "{{name}} is {name}", "label": "{{x}} {name}", "clock": "{%H}}",
	})
	got := []string{render(t, c, "greeting", ada), render(t, c, "label", nil), render(t, c, "clock", nil)}
	if want := []string{"{name} is Ada", "{{x}} {name}", "{%H}}"}; !slices.Equal(got, want) {
		t.Errorf("rendered %q, want %q", got, want)
	}
}

func TestProfileFilesAreUsedVerbatim(t *testing.T) {
	text := "\ufeff  Hello, {name}. \r\n\n"
	c := load(t, entries, profile(t, map[string]string{"greeting.txt": text}))
	if c.Text("greeting") != text {
		t.Errorf("text %q, want %q", c.Text("greeting"), text)
	}
	if got, want := render(t, c, "greeting", ada), "\ufeff  Hello, Ada. \r\n\n"; got != want {
		t.Errorf("rendered %q, want %q", got, want)
	}
}

func TestOnlyTxtFilesInTheProfileDirectoryAreRead(t *testing.T) {
	dir := profile(t, map[string]string{
		"README.md": "{", "label.txt.orig": "{", "label": "{", "fixtures/label.txt": "fixture",
	})
	if got := load(t, entries, dir).Text("label"); got != "{label}" {
		t.Errorf("label %q, want the default", got)
	}
}

func TestLoadReportsEveryProblem(t *testing.T) {
	dir := profile(t, map[string]string{
		"greeting.txt": "Hello,\n {nme}.",
		"label.txt":    "bad \xff byte",
		"unknown.txt":  "text",
	})
	_, err := prompt.Load(slices.Concat(entries, entries[1:2]), dir)
	want := "invalid prompts:\n" +
		"label: declared twice\n" +
		dir + `/greeting.txt: prompt: line 2, column 2 (offset 8): unknown placeholder "{nme}"; write {{ and }} for literal braces` + "\n" +
		dir + "/label.txt: not valid UTF-8\n" +
		dir + "/unknown.txt: unknown ID"
	if err == nil || err.Error() != want {
		t.Errorf("error:\n%v\nwant:\n%s", err, want)
	}
}

func TestMissingProfileDirectoryFailsToLoad(t *testing.T) {
	_, err := prompt.Load(entries, filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error %v, want one for the missing directory", err)
	}
}

func TestInvalidOverridesReportEveryProblemAndLeaveTheCatalogUnchanged(t *testing.T) {
	c := load(t, entries, "")
	_, err := c.With(map[string]string{"nope": "text", "greeting": "{name", "label": "\xff"})
	want := "invalid prompts:\n" +
		`prompts["greeting"]: prompt: line 1, column 1 (offset 0): unmatched '{'; write {{ and }} for literal braces` + "\n" +
		`prompts["label"]: not valid UTF-8` + "\n" +
		`prompts["nope"]: unknown ID`
	if err == nil || err.Error() != want {
		t.Errorf("error:\n%v\nwant:\n%s", err, want)
	}
	if got := render(t, c, "greeting", ada); got != "Hello, Ada." {
		t.Errorf("greeting after a failed override renders %q", got)
	}
}

func TestTemplatesDoNotRereadProfileFiles(t *testing.T) {
	dir := profile(t, map[string]string{"greeting.txt": "Hi {name}"})
	c := load(t, entries, dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if got := render(t, c, "greeting", ada); got != "Hi Ada" {
		t.Errorf("rendered %q after the profile was removed", got)
	}
}

func TestUnknownEntryLookupPanics(t *testing.T) {
	defer func() {
		if r := recover(); r != `prompt: no catalog entry "nope"` {
			t.Errorf("recovered %v", r)
		}
	}()
	load(t, entries, "").Template("nope")
}

func TestExportedCatalogLoadsBackIdentically(t *testing.T) {
	builtin := prompt.Builtin()
	c := load(t, builtin, profile(t, map[string]string{"error.generic.txt": "Profile apology.\n"}))
	c = with(t, c, map[string]string{"system.no_memory": "Nothing {remembered}."})
	out := filepath.Join(t.TempDir(), "new-agent")
	if err := c.Export(out, false); err != nil {
		t.Fatal(err)
	}

	files, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(builtin) {
		t.Errorf("exported %d files, want %d", len(files), len(builtin))
	}
	back := load(t, builtin, out)
	for _, e := range builtin {
		b, err := os.ReadFile(filepath.Join(out, e.ID+".txt"))
		if err != nil || string(b) != c.Text(e.ID) {
			t.Errorf("%s.txt: %q, %v; want %q", e.ID, b, err, c.Text(e.ID))
		}
		if back.Text(e.ID) != c.Text(e.ID) {
			t.Errorf("%s loaded back as %q, want %q", e.ID, back.Text(e.ID), c.Text(e.ID))
		}
	}
}

func TestExportOverwritesOnlyWhenForced(t *testing.T) {
	c := load(t, entries, "")
	out := profile(t, map[string]string{"label.txt": "mine", "clock.txt": "mine"})

	err := c.Export(out, false)
	want := "prompt: refusing to overwrite existing files: " + out + "/clock.txt, " + out + "/label.txt"
	if err == nil || err.Error() != want {
		t.Errorf("error %v, want %q", err, want)
	}
	if _, err := os.Stat(filepath.Join(out, "greeting.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("greeting.txt was written by a refused export: %v", err)
	}

	if err := c.Export(out, true); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"greeting": "Hello, {name}.", "label": "{label}", "clock": "%H:%M"} {
		if b, err := os.ReadFile(filepath.Join(out, id+".txt")); err != nil || string(b) != want {
			t.Errorf("%s.txt: %q, %v; want %q", id, b, err, want)
		}
	}
}
