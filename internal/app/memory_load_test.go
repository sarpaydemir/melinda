//go:build load

package app_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"testing"

	"github.com/sarpaydemir/melinda/internal/adapters/httpfetch"
	"github.com/sarpaydemir/melinda/internal/adapters/openai"
	"github.com/sarpaydemir/melinda/internal/clock"
	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/files"
	"github.com/sarpaydemir/melinda/internal/llm"
	"github.com/sarpaydemir/melinda/internal/port/chat"
	"github.com/sarpaydemir/melinda/internal/prompt"
)

// containerLimit is the default memory limit of the agent's container in deploy/compose.yml.
const containerLimit = 1 << 30

// TestWorstCaseRepliesStayUnderTheContainerLimit runs as many replies at once as the pipeline does by default, each
// with as many attachments as a message may have: images and a PDF of the largest size, and workbooks built to make
// text extraction hold the most memory, which the vision model fails to read. Alongside each reply, a 5 MiB page of
// one-letter paragraphs, whose parse tree is about 80 times its size, is fetched. The soft memory limit is the one
// the agent sets in that container.
func TestWorstCaseRepliesStayUnderTheContainerLimit(t *testing.T) {
	const replies, size = 8, 20 << 20
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(containerLimit / 10 * 9))

	visionModel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"[ERR_UNREADABLE]"}}]}`)
	}))
	defer visionModel.Close()
	page := []byte(strings.Repeat("<p>x", 5<<20/4))
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(page)
	}))
	defer site.Close()

	s := loadSettings(t, map[string]any{"model": "m", "vision_model": "v", "api_base": visionModel.URL, "key": "k",
		"web_fetch_allow_private": true})
	log := slog.New(slog.DiscardHandler)
	provider, err := openai.New(s, http.DefaultTransport, clock.Real(), "melinda/test", log)
	if err != nil {
		t.Fatal(err)
	}
	fetcher, err := httpfetch.New(s)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := prompt.Load(prompt.Builtin(), "")
	if err != nil {
		t.Fatal(err)
	}
	p := &generated{book: hostileWorkbook(t), files: map[string]chat.Attachment{
		"1": {Name: "photo.png", Size: size}, "2": {Name: "book.xlsx"}, "3": {Name: "scan.pdf", Size: size},
		"4": {Name: "sheet.xlsm"}, "5": {Name: "photo.webp", Size: size},
	}}
	d := files.Describer{Platform: p, Settings: s, Catalog: catalog, Log: log,
		Model: llm.Client{Provider: provider, Failure: "Failed.", Log: log}}
	sections := []string{"Analysis of photo.png\n[ERR_UNREADABLE]", "Analysis of book.xlsx\nText extracted",
		"Analysis of scan.pdf\n[ERR_UNREADABLE]", "Analysis of sheet.xlsm\nText extracted",
		"Analysis of photo.webp\n[ERR_UNREADABLE]"}

	resetPeak(t)
	var wg sync.WaitGroup
	for range replies {
		wg.Go(func() {
			fileCtx, _ := d.Describe(t.Context(), chat.Message{Text: "?", AttachmentIDs: []string{"1", "2", "3", "4", "5"}})
			for _, want := range sections {
				if !strings.Contains(fileCtx, want) {
					t.Errorf("file context %.300q lacks %q", fileCtx, want)
				}
			}
		})
		wg.Go(func() {
			text, err := fetcher.Fetch(t.Context(), site.URL)
			if want := "Title: No Title\nURL: " + site.URL + "\n\nx\nx\n"; err != nil || !strings.HasPrefix(text, want) {
				t.Errorf("Fetch = %.40q, %v; want the page's text", text, err)
			}
		})
	}
	wg.Wait()
	peak := peakRSS(t)
	t.Logf("peak RSS %d MiB with a %d MiB limit", peak>>20, containerLimit>>20)
	if peak >= containerLimit {
		t.Errorf("peak RSS %d MiB, want under %d MiB", peak>>20, containerLimit>>20)
	}
}

// generated serves attachments, reading each download the way a platform adapter reads a response body: a
// workbook for the documents, zeros of the attachment's size for the rest.
type generated struct {
	chat.Platform
	book  []byte
	files map[string]chat.Attachment
}

func (p *generated) Attachment(_ context.Context, id string) (chat.Attachment, error) {
	a := p.files[id]
	if a.Size == 0 {
		a.Size = int64(len(p.book))
	}
	return a, nil
}

func (p *generated) Download(_ context.Context, id string, max int64) ([]byte, error) {
	if a := p.files[id]; a.Size > 0 {
		return io.ReadAll(io.LimitReader(zeros{}, min(a.Size, max)))
	}
	return io.ReadAll(bytes.NewReader(p.book))
}

type zeros struct{}

func (zeros) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

// hostileWorkbook is a workbook within the extraction limits that makes extraction hold the most memory: a shared
// string of 60 MiB, and a worksheet that refers to it and has 4 MiB values.
func hostileWorkbook(t *testing.T) []byte {
	t.Helper()
	run := strings.Repeat("x", 4<<20-8192)
	var shared, cells strings.Builder
	for shared.Len()+len(run) < 60<<20 {
		shared.WriteString("<t>" + run + "</t>")
	}
	for cells.Len()+len(run) < 64<<20-1024 {
		cells.WriteString("<c><v>" + run + "</v></c>")
	}
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for _, e := range []struct{ name, body string }{
		{"xl/sharedStrings.xml", "<sst><si>" + shared.String() + "</si></sst>"},
		{"xl/worksheets/sheet1.xml", `<w><c t="s"><v>0</v></c>` + cells.String() + "</w>"},
	} {
		f, err := w.Create(e.name)
		if err == nil {
			_, err = io.WriteString(f, e.body)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func loadSettings(t *testing.T, doc map[string]any) *config.Settings {
	t.Helper()
	m := map[string]json.RawMessage{}
	for k, v := range doc {
		m[k], _ = json.Marshal(v)
	}
	s, err := config.Load(m, "agent", nil, func(*config.Settings) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// resetPeak starts the process's peak resident set size over from its current size.
func resetPeak(t *testing.T) {
	t.Helper()
	debug.FreeOSMemory()
	if err := os.WriteFile("/proc/self/clear_refs", []byte("5"), 0); err != nil {
		t.Skipf("cannot reset the peak resident set size: %v", err)
	}
}

// peakRSS is the process's peak resident set size in bytes.
func peakRSS(t *testing.T) int64 {
	t.Helper()
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(status)) {
		if kb, ok := strings.CutPrefix(line, "VmHWM:"); ok {
			var n int64
			if _, err := fmt.Sscan(kb, &n); err != nil {
				t.Fatal(err)
			}
			return n << 10
		}
	}
	t.Fatal("no VmHWM in /proc/self/status")
	return 0
}
