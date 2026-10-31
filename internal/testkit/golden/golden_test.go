package golden

import (
	"os"
	"strings"
	"testing"
)

type failures struct {
	testing.TB
	msgs []string
}

func (f *failures) Errorf(format string, args ...any) { f.msgs = append(f.msgs, format) }

// updating turns on -update until the caller resets it or the test ends.
func updating(t *testing.T) {
	*update = true
	t.Cleanup(func() { *update = false })
}

func TestUpdateWritesThenAssertCompares(t *testing.T) {
	t.Chdir(t.TempDir())
	updating(t)
	Assert(t, "nested/reply", []byte("line 1\nİstanbul\n"))
	*update = false

	Assert(t, "nested/reply", []byte("line 1\nİstanbul\n"))

	f := &failures{TB: t}
	Assert(f, "nested/reply", []byte("line 1\nistanbul\n"))
	Assert(f, "nested/reply", []byte("line 1\nİstanbul\n\n"))
	if len(f.msgs) != 2 || !strings.Contains(f.msgs[0], "line %d") {
		t.Errorf("failures = %q, want a line mismatch and a length mismatch", f.msgs)
	}
}

func TestAssertJSONComparesValues(t *testing.T) {
	t.Chdir(t.TempDir())
	updating(t)
	AssertJSON(t, "body", []byte(`{"model":"m","temperature":0.7}`))
	*update = false

	written, err := os.ReadFile(file("body"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"model\": \"m\",\n  \"temperature\": 0.7\n}\n"; string(written) != want {
		t.Errorf("golden file = %q, want %q", written, want)
	}

	AssertJSON(t, "body", []byte(`{"temperature":0.7, "model":"m"}`))
	f := &failures{TB: t}
	AssertJSON(f, "body", []byte(`{"model":"m","temperature":0.1}`))
	if len(f.msgs) != 1 {
		t.Errorf("failures = %d, want 1", len(f.msgs))
	}
}
