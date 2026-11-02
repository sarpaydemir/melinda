// Package golden compares test output with files in testdata; `go test -update` rewrites them.
package golden

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sarpaydemir/melinda/internal/testkit"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden files with the current output")

func file(name string) string { return filepath.Join("testdata", name+".golden") }

// Assert fails t unless got matches the golden file byte for byte.
func Assert(t testing.TB, name string, got []byte) {
	t.Helper()
	want := read(t, name, got)
	if bytes.Equal(got, want) {
		return
	}
	gl, wl := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := range min(len(gl), len(wl)) {
		if gl[i] != wl[i] {
			t.Errorf("%s differs at line %d:\n got: %q\nwant: %q", file(name), i+1, gl[i], wl[i])
			return
		}
	}
	t.Errorf("%s differs: got %d lines, want %d", file(name), len(gl), len(wl))
}

// AssertJSON fails t unless got is the same JSON value as the golden file (see testkit.JSONEqual). With
// -update it writes got indented.
func AssertJSON(t testing.TB, name string, got []byte) {
	t.Helper()
	var indented bytes.Buffer
	if err := json.Indent(&indented, got, "", "  "); err != nil {
		t.Fatalf("%s: output is not JSON: %v\n%s", file(name), err, got)
	}
	indented.WriteByte('\n')
	want := read(t, name, indented.Bytes())
	eq, err := testkit.JSONEqual(want, got)
	if err != nil {
		t.Fatalf("%s: %v", file(name), err)
	}
	if !eq {
		t.Errorf("%s differs:\n got: %s\nwant: %s", file(name), got, want)
	}
}

// read returns the golden file, first writing content to it when -update is set.
func read(t testing.TB, name string, content []byte) []byte {
	t.Helper()
	path := file(name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	return want
}
