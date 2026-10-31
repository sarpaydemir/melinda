package prompt

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Kind says how an entry's text is used.
type Kind int

const (
	// KindFormat text is a template whose placeholders are the entry's.
	KindFormat Kind = iota
	// KindRaw text is used verbatim.
	KindRaw
	// KindTime text is a strftime layout, used verbatim.
	KindTime
)

// Entry declares a catalog entry.
type Entry struct {
	ID           string
	Kind         Kind
	Placeholders []string
	Default      string
}

// Catalog holds the text of every entry, compiled. It is immutable and safe for concurrent use.
type Catalog struct {
	items map[string]item
}

type item struct {
	entry Entry
	text  string
	tmpl  *Template
}

// Load declares entries with their default texts, then replaces the texts that dir, when set, holds as <id>.txt
// files. Files are read once and used byte for byte; other files in dir are ignored. The error lists every problem.
func Load(entries []Entry, dir string) (*Catalog, error) {
	c := &Catalog{items: make(map[string]item, len(entries))}
	var errs []error
	for _, e := range entries {
		if _, dup := c.items[e.ID]; dup {
			errs = append(errs, fmt.Errorf("%s: declared twice", e.ID))
			continue
		}
		c.items[e.ID] = item{entry: e}
		errs = append(errs, c.set("default "+e.ID, e.ID, e.Default))
	}
	if dir != "" {
		errs = append(errs, c.read(dir)...)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("invalid prompts:\n%w", err)
	}
	return c, nil
}

// With returns a catalog in which overrides, the prompts setting, replace the texts of the entries they name.
// The error lists every problem; c is never changed.
func (c *Catalog) With(overrides map[string]string) (*Catalog, error) {
	if len(overrides) == 0 {
		return c, nil
	}
	next := &Catalog{items: maps.Clone(c.items)}
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(overrides)) {
		errs = append(errs, next.set(fmt.Sprintf("prompts[%q]", id), id, overrides[id]))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("invalid prompts:\n%w", err)
	}
	return next, nil
}

// Template returns the compiled text of the entry id. It panics when no entry has that ID.
func (c *Catalog) Template(id string) *Template { return c.item(id).tmpl }

// Text returns the text of the entry id as loaded, before rendering. It panics when no entry has that ID.
func (c *Catalog) Text(id string) string { return c.item(id).text }

// Render fills the entry id with values, which must cover every placeholder the entry declares; it panics
// otherwise, or when no entry has that ID.
func (c *Catalog) Render(id string, values map[string]string) string {
	s, err := c.Template(id).Render(values)
	if err != nil {
		panic(err) // the catalog admits only declared placeholders, so a miss is a programming error
	}
	return s
}

// Export writes the text of every entry to dir as <id>.txt, creating dir when needed. Unless force is set, it
// writes nothing when any of those files exists.
func (c *Catalog) Export(dir string, force bool) error {
	ids := slices.Sorted(maps.Keys(c.items))
	if !force {
		var existing []string
		for _, id := range ids {
			path := filepath.Join(dir, id+".txt")
			_, err := os.Lstat(path)
			switch {
			case err == nil:
				existing = append(existing, path)
			case !errors.Is(err, fs.ErrNotExist):
				return err
			}
		}
		if len(existing) > 0 {
			return fmt.Errorf("prompt: refusing to overwrite existing files: %s", strings.Join(existing, ", "))
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, id := range ids {
		if err := os.WriteFile(filepath.Join(dir, id+".txt"), []byte(c.items[id].text), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) item(id string) item {
	it, ok := c.items[id]
	if !ok {
		panic("prompt: no catalog entry " + strconv.Quote(id))
	}
	return it
}

// read sets the texts of the <id>.txt files in dir.
func (c *Catalog) read(dir string) []error {
	files, err := os.ReadDir(dir)
	if err != nil {
		return []error{err}
	}
	var errs []error
	for _, f := range files {
		id, ok := strings.CutSuffix(f.Name(), ".txt")
		if !ok {
			continue
		}
		path := filepath.Join(dir, f.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, c.set(path, id, string(b)))
	}
	return errs
}

// set makes text the text of the entry id, compiled for its kind; source names the text in errors.
func (c *Catalog) set(source, id, text string) error {
	it, ok := c.items[id]
	switch {
	case !ok:
		return fmt.Errorf("%s: unknown ID", source)
	case !utf8.ValidString(text):
		return fmt.Errorf("%s: not valid UTF-8", source)
	}
	it.text, it.tmpl = text, Raw(text)
	if it.entry.Kind == KindFormat {
		tmpl, err := Parse(text, it.entry.Placeholders)
		if err != nil {
			return fmt.Errorf("%s: %w", source, err)
		}
		it.tmpl = tmpl
	}
	c.items[id] = it
	return nil
}
