package files_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/files"
	"github.com/sarpaydemir/melinda/internal/port/chat"
)

// stored is an attachment as the fake platform keeps it.
type stored struct {
	meta        chat.Attachment
	body        string
	metaErr     error
	downloadErr error
}

// platform serves attachments from memory, enforces the download limit, and records its calls.
type platform struct {
	chat.Platform
	files map[string]stored
	calls []string
}

func (p *platform) Attachment(_ context.Context, id string) (chat.Attachment, error) {
	p.calls = append(p.calls, "attachment "+id)
	f, ok := p.files[id]
	if !ok {
		return chat.Attachment{}, errors.New("no such file")
	}
	return f.meta, f.metaErr
}

func (p *platform) Download(_ context.Context, id string, max int64) ([]byte, error) {
	p.calls = append(p.calls, fmt.Sprintf("download %s %d", id, max))
	f := p.files[id]
	switch {
	case f.downloadErr != nil:
		return nil, f.downloadErr
	case int64(len(f.body)) > max:
		return nil, fmt.Errorf("download %s: %w", id, chat.ErrTooLarge)
	}
	return []byte(f.body), nil
}

// analyse records the file it is given and returns a note naming it.
func (p *platform) analyse(_ context.Context, f files.File) files.Note {
	p.calls = append(p.calls, fmt.Sprintf("analyse %s %q", f.Name, f.Data))
	return files.Note{ID: "analysed", Values: map[string]string{"filename": f.Name}}
}

var discard = slog.New(slog.DiscardHandler)

const withVision = `{"model": "m", "vision_model": "v"}`

func settings(t *testing.T, doc string) *config.Settings {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatal(err)
	}
	s, err := config.Load(m, "agent", nil, func(*config.Settings) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// describe lists a context's notes, then its summary parts, one line each.
func describe(c files.Context) []string {
	var out []string
	for _, n := range c.Notes {
		line := n.ID
		for _, k := range slices.Sorted(maps.Keys(n.Values)) {
			line += " " + k + "=" + n.Values[k]
		}
		out = append(out, line)
	}
	for _, p := range c.Parts {
		out = append(out, fmt.Sprintf("part %s (%s)", p.Name, p.Kind))
	}
	return out
}

func check(t *testing.T, got files.Context, want ...string) {
	t.Helper()
	if lines := describe(got); !slices.Equal(lines, want) {
		t.Errorf("context:\n got %s\nwant %s", strings.Join(lines, "\n     "), strings.Join(want, "\n     "))
	}
}

// note ingests one attachment with the given metadata and returns the note it produced.
func note(t *testing.T, meta chat.Attachment) string {
	t.Helper()
	p := &platform{files: map[string]stored{"f1": {meta: meta, body: "data"}}}
	c := files.Ingest(t.Context(), p, settings(t, withVision), discard, []string{"f1"}, p.analyse)
	if len(c.Notes) != 1 {
		t.Fatalf("%+v: got notes %q, want one", meta, describe(c))
	}
	return describe(c)[0]
}

func TestNoAttachmentsDoNothing(t *testing.T) {
	for _, ids := range [][]string{nil, {}} {
		p := &platform{}
		got := files.Ingest(t.Context(), p, settings(t, `{"model": "m"}`), discard, ids, p.analyse)
		if !reflect.DeepEqual(got, files.Context{}) {
			t.Errorf("Ingest(%q) = %+v, want nothing", ids, got)
		}
		if len(p.calls) > 0 {
			t.Errorf("Ingest(%q) called %q", ids, p.calls)
		}
	}
}

func TestAnalysableAttachmentsAreDownloaded(t *testing.T) {
	for _, meta := range []chat.Attachment{
		{Name: "a.jpg"}, {Name: "a.jpeg"}, {Name: "a.png"}, {Name: "a.webp"}, {Name: "a.JPG"},
		{Name: "photo", MediaType: "image/jpeg"}, {Name: "photo", MediaType: "image/png"},
		{Name: "photo", MediaType: "image/webp"},
		{Name: "a.pdf"}, {Name: "scan", Extension: "PDF"}, {Name: "scan", MediaType: "application/pdf"},
		{Name: "a.doc"}, {Name: "a.docx"}, {Name: "a.xls"}, {Name: "a.xlsx"}, {Name: "a.xlsm"}, {Name: "a.ppt"},
		{Name: "a.pptx"}, {Name: "a.odt"}, {Name: "a.ods"}, {Name: "a.odp"},
		{Name: "a.txt"}, {Name: "a.md"}, {Name: "a.csv"}, {Name: "a.json"}, {Name: "a.log"}, {Name: "a.xml"},
		{Name: "a.yaml"}, {Name: "a.yml"},
		{Name: "main", MediaType: "text/x-go"}, {Name: "page", MediaType: "text/html; charset=utf-8"},
	} {
		if got, want := note(t, meta), "analysed filename="+meta.Name; got != want {
			t.Errorf("%+v: got %q, want %q", meta, got, want)
		}
	}
}

func TestAnalysableMediaTypeOverridesTheExtension(t *testing.T) {
	for _, meta := range []chat.Attachment{
		{Name: "a.bin", MediaType: "image/png"},
		{Name: "a.zip", MediaType: "application/pdf"},
		{Name: "a.exe", Extension: "exe", MediaType: "text/plain"},
	} {
		if got, want := note(t, meta), "analysed filename="+meta.Name; got != want {
			t.Errorf("%+v: got %q, want %q", meta, got, want)
		}
	}
}

func TestUnsupportedAttachmentsAreNamedByExtensionThenMediaType(t *testing.T) {
	for _, tc := range []struct {
		meta chat.Attachment
		want string
	}{
		{chat.Attachment{Name: "archive.zip", MediaType: "application/zip"}, "kind=.zip"},
		{chat.Attachment{Name: "anim.gif", MediaType: "image/gif"}, "kind=.gif"},
		{chat.Attachment{Name: "archive", MediaType: "application/zip"}, "kind=application/zip"},
		{chat.Attachment{Name: "blob"}, "kind=application/octet-stream"},
		{chat.Attachment{Name: "blob", Extension: "."}, "kind=."},
		{chat.Attachment{Name: "photo", MediaType: "image/PNG"}, "kind=image/PNG"},
		{chat.Attachment{Name: "report", MediaType: "application/msword"}, "kind=application/msword"},
		{chat.Attachment{Name: "sheet", MediaType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
			"kind=application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
	} {
		if got, want := note(t, tc.meta), "files.unsupported filename="+tc.meta.Name+" "+tc.want; got != want {
			t.Errorf("%+v: got %q, want %q", tc.meta, got, want)
		}
	}
}

func TestExtensionComesFromMetadataThenName(t *testing.T) {
	for _, tc := range []struct {
		meta chat.Attachment
		kind string
	}{
		{chat.Attachment{Name: "test.PNG"}, "png"},
		{chat.Attachment{Name: "test.XLSX"}, "xlsx"},
		{chat.Attachment{Name: "backup.tar.GZ"}, "gz"},
		{chat.Attachment{Name: ".env"}, "env"},
		{chat.Attachment{Name: "README"}, ""},
		{chat.Attachment{Name: "notes."}, ""},
		{chat.Attachment{Name: "x.png", Extension: "JPG"}, "jpg"},
		{chat.Attachment{Name: "x", Extension: "..Pdf"}, "pdf"},
		{chat.Attachment{Name: "x.png", Extension: "."}, ""},
		{chat.Attachment{Name: "resim.İMG"}, "i̇mg"},
	} {
		p := &platform{files: map[string]stored{"f1": {meta: tc.meta}}}
		c := files.Ingest(t.Context(), p, settings(t, `{"model": "m"}`), discard, []string{"f1"}, p.analyse)
		if want := []files.Part{{Name: tc.meta.Name, Kind: tc.kind}}; !reflect.DeepEqual(c.Parts, want) {
			t.Errorf("%+v: got parts %+v, want %+v", tc.meta, c.Parts, want)
		}
	}
}

func TestUnnamedAttachmentIsCalledByItsID(t *testing.T) {
	p := &platform{files: map[string]stored{"f1": {meta: chat.Attachment{Extension: "png", Size: 4}, body: "data"}}}
	check(t, files.Ingest(t.Context(), p, settings(t, withVision), discard, []string{"f1"}, p.analyse),
		"analysed filename=file_f1", "part file_f1 (png)")
}

func TestAnalysisGetsTheDownloadedFileWithItsMetadata(t *testing.T) {
	meta := chat.Attachment{Name: "Rapor.PDF", Extension: "pdf", MediaType: "application/pdf", Size: 4}
	p := &platform{files: map[string]stored{"f1": {meta: meta, body: "%PDF"}}}
	var got []files.File
	analyse := func(_ context.Context, f files.File) files.Note {
		got = append(got, f)
		return files.Note{ID: "analysed"}
	}
	c := files.Ingest(t.Context(), p, settings(t, withVision), discard, []string{"f1"}, analyse)
	want := []files.File{{Name: "Rapor.PDF", Attachment: meta, Data: []byte("%PDF")}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("analysed %+v, want %+v", got, want)
	}
	check(t, c, "analysed", "part Rapor.PDF (pdf)")
	if want := []string{"attachment f1", "download f1 20971520"}; !slices.Equal(p.calls, want) {
		t.Errorf("calls %q, want %q", p.calls, want)
	}
}

func TestEachFileIsAnalysedBeforeTheNextDownload(t *testing.T) {
	p := &platform{files: map[string]stored{
		"f1": {meta: chat.Attachment{Name: "a.png"}, body: "1"},
		"f2": {meta: chat.Attachment{Name: "b.pdf"}, body: "2"},
		"f3": {meta: chat.Attachment{Name: "c.txt"}, body: "3"},
	}}
	check(t, files.Ingest(t.Context(), p, settings(t, withVision), discard, []string{"f1", "f2", "f3"}, p.analyse),
		"analysed filename=a.png", "analysed filename=b.pdf", "analysed filename=c.txt",
		"part a.png (png)", "part b.pdf (pdf)", "part c.txt (txt)")
	if want := []string{
		"attachment f1", "download f1 20971520", `analyse a.png "1"`,
		"attachment f2", "download f2 20971520", `analyse b.pdf "2"`,
		"attachment f3", "download f3 20971520", `analyse c.txt "3"`,
	}; !slices.Equal(p.calls, want) {
		t.Errorf("calls %q, want %q", p.calls, want)
	}
}

func TestReportedSizeOverTheLimitSkipsTheDownload(t *testing.T) {
	p := &platform{files: map[string]stored{
		"f1": {meta: chat.Attachment{Name: "big.pdf", Size: 20971521}},
		"f2": {meta: chat.Attachment{Name: "edge.pdf", Size: 20971520}, body: "data"},
	}}
	check(t, files.Ingest(t.Context(), p, settings(t, withVision), discard, []string{"f1", "f2"}, p.analyse),
		"files.too_large filename=big.pdf max_mb=20", "analysed filename=edge.pdf",
		"part big.pdf (pdf)", "part edge.pdf (pdf)")
	want := []string{"attachment f1", "attachment f2", "download f2 20971520", `analyse edge.pdf "data"`}
	if !slices.Equal(p.calls, want) {
		t.Errorf("calls %q, want %q", p.calls, want)
	}
}

func TestSizeNoteGivesTheLimitInWholeMegabytes(t *testing.T) {
	for limit, mb := range map[int]string{20000000: "20", 20971520: "20", 1999999: "1", 999999: "0"} {
		p := &platform{files: map[string]stored{"f1": {meta: chat.Attachment{Name: "big.pdf", Size: 1 << 30}}}}
		s := settings(t, fmt.Sprintf(`{"model": "m", "vision_model": "v", "file_max_bytes": %d}`, limit))
		if got, want := describe(files.Ingest(t.Context(), p, s, discard, []string{"f1"}, p.analyse))[0],
			"files.too_large filename=big.pdf max_mb="+mb; got != want {
			t.Errorf("limit %d: got %q, want %q", limit, got, want)
		}
	}
}

func TestBodyLargerThanItsReportedSizeIsRejected(t *testing.T) {
	p := &platform{files: map[string]stored{
		"f1": {meta: chat.Attachment{Name: "small.png", Size: 10}, body: strings.Repeat("x", 3_000_001)},
	}}
	s := settings(t, `{"model": "m", "vision_model": "v", "file_max_bytes": 3000000}`)
	check(t, files.Ingest(t.Context(), p, s, discard, []string{"f1"}, p.analyse),
		"files.too_large filename=small.png max_mb=3", "part small.png (png)")
	if want := []string{"attachment f1", "download f1 3000000"}; !slices.Equal(p.calls, want) {
		t.Errorf("calls %q, want %q", p.calls, want)
	}
}

func TestMetadataFailureIsNotedAndTheNextFileProcessed(t *testing.T) {
	p := &platform{files: map[string]stored{
		"f1": {metaErr: errors.New("timeout")},
		"f2": {},
		"f3": {meta: chat.Attachment{Name: "a.txt", Size: 4}, body: "data"},
	}}
	check(t, files.Ingest(t.Context(), p, settings(t, withVision), discard, []string{"f1", "f2", "f3"}, p.analyse),
		"files.meta_failed file_id=f1", "files.meta_failed file_id=f2", "analysed filename=a.txt", "part a.txt (txt)")
}

func TestDownloadFailureIsNoted(t *testing.T) {
	p := &platform{files: map[string]stored{
		"f1": {meta: chat.Attachment{Name: "a.png", Size: 4}, downloadErr: errors.New("status 500")},
	}}
	check(t, files.Ingest(t.Context(), p, settings(t, withVision), discard, []string{"f1"}, p.analyse),
		"files.download_failed filename=a.png", "part a.png (png)")
}

func TestWithoutVisionModelFilesAreOnlySummarized(t *testing.T) {
	for _, doc := range []string{
		`{"model": "m"}`, `{"model": "m", "vision_model": ""}`, `{"model": "m", "vision_model": null}`,
	} {
		p := &platform{files: map[string]stored{
			"f1": {meta: chat.Attachment{Name: "a.png", Size: 1 << 40}},
			"f3": {meta: chat.Attachment{Name: "b.zip"}},
		}}
		check(t, files.Ingest(t.Context(), p, settings(t, doc), discard, []string{"f1", "f2", "f3"}, p.analyse),
			"files.meta_failed file_id=f2", "files.no_vision", "part a.png (png)", "part b.zip (zip)")
		if want := []string{"attachment f1", "attachment f2", "attachment f3"}; !slices.Equal(p.calls, want) {
			t.Errorf("%s: calls %q, want %q", doc, p.calls, want)
		}
	}
}

func TestOnlyTheFirstFilesWithinTheCountAreProcessed(t *testing.T) {
	ids := []string{"f1", "f2", "f3", "f4", "f5", "f6", "f7"}
	p := &platform{files: map[string]stored{
		"f1": {meta: chat.Attachment{Name: "a.png", Size: 4}, body: "data"},
		"f2": {meta: chat.Attachment{Name: "b.zip"}},
		"f4": {meta: chat.Attachment{Name: "c.pdf", Size: 1 << 30}},
		"f5": {meta: chat.Attachment{Name: "d.md", Size: 2}, downloadErr: errors.New("reset")},
		"f6": {meta: chat.Attachment{Name: "e.png", Size: 4}, body: "data"},
		"f7": {meta: chat.Attachment{Name: "f.png", Size: 4}, body: "data"},
	}}
	check(t, files.Ingest(t.Context(), p, settings(t, withVision), discard, ids, p.analyse),
		"files.too_many count=2",
		"analysed filename=a.png",
		"files.unsupported filename=b.zip kind=.zip",
		"files.meta_failed file_id=f3",
		"files.too_large filename=c.pdf max_mb=20",
		"files.download_failed filename=d.md",
		"part a.png (png)", "part b.zip (zip)", "part c.pdf (pdf)", "part d.md (md)")
	if want := []string{
		"attachment f1", "download f1 20971520", `analyse a.png "data"`, "attachment f2", "attachment f3",
		"attachment f4", "attachment f5", "download f5 20971520",
	}; !slices.Equal(p.calls, want) {
		t.Errorf("calls %q, want %q", p.calls, want)
	}
}

func TestFileCountLimitIsASetting(t *testing.T) {
	p := &platform{files: map[string]stored{"f1": {meta: chat.Attachment{Name: "a.png"}}}}
	s := settings(t, `{"model": "m", "file_max_count": 1}`)
	check(t, files.Ingest(t.Context(), p, s, discard, []string{"f1", "f2", "f3"}, p.analyse),
		"files.too_many count=2", "files.no_vision", "part a.png (png)")
}

func TestFailuresAreLoggedWithTheirCause(t *testing.T) {
	p := &platform{files: map[string]stored{
		"f1": {metaErr: errors.New("metadata timeout")},
		"f2": {meta: chat.Attachment{Name: "a.png"}, downloadErr: errors.New("status 502")},
		"f3": {},
	}}
	var out strings.Builder
	log := slog.New(slog.NewTextHandler(&out, nil))
	files.Ingest(t.Context(), p, settings(t, withVision), log, []string{"f1", "f2", "f3"}, p.analyse)
	for _, want := range []string{
		`file_id=f1 error="metadata timeout"`, `file_id=f2 error="status 502"`, `file_id=f3 error="empty metadata"`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("log %q lacks %q", out.String(), want)
		}
	}
}

// shared serves one attachment to concurrent replies and counts its downloads.
type shared struct {
	chat.Platform
	meta      chat.Attachment
	downloads atomic.Int32
}

func (p *shared) Attachment(context.Context, string) (chat.Attachment, error) { return p.meta, nil }

func (p *shared) Download(context.Context, string, int64) ([]byte, error) {
	p.downloads.Add(1)
	return []byte("data"), nil
}

// analysed is an analysis that returns the note named id.
func analysed(id string) func(context.Context, files.File) files.Note {
	return func(context.Context, files.File) files.Note { return files.Note{ID: id} }
}

// holding starts a reply whose attachment needs all the memory attachments may use and returns once the
// attachment's analysis has started. The function it returns ends that analysis and returns the reply's context.
func holding(t *testing.T, p *shared, s *config.Settings) func() files.Context {
	t.Helper()
	analysing, finish, done := make(chan struct{}), make(chan struct{}), make(chan files.Context)
	go func() {
		done <- files.Ingest(t.Context(), p, s, discard, []string{"f1"}, func(context.Context, files.File) files.Note {
			close(analysing)
			<-finish
			return files.Note{ID: "first"}
		})
	}()
	select {
	case <-analysing:
	case <-time.After(5 * time.Second):
		t.Fatal("the first attachment was not analysed")
	}
	return sync.OnceValue(func() files.Context {
		close(finish)
		return <-done
	})
}

// hugeFile is an attachment whose size alone needs more memory than attachments may use together.
var hugeFile = chat.Attachment{Name: "huge.png", Size: 1 << 40}

const hugeLimit = `{"model": "m", "vision_model": "v", "file_max_bytes": 1099511627776}`

func TestAttachmentWaitsForTheMemoryAnotherReplyHolds(t *testing.T) {
	p := &shared{meta: hugeFile}
	s := settings(t, hugeLimit)
	release := holding(t, p, s)
	defer release()
	second := make(chan files.Context)
	go func() { second <- files.Ingest(t.Context(), p, s, discard, []string{"f1"}, analysed("second")) }()
	select {
	case c := <-second:
		t.Fatalf("second reply ingested %q while the first held the memory", describe(c))
	case <-time.After(50 * time.Millisecond):
	}
	if n := p.downloads.Load(); n != 1 {
		t.Errorf("%d downloads while the first attachment was analysed, want 1", n)
	}
	check(t, release(), "first", "part huge.png (png)")
	check(t, <-second, "second", "part huge.png (png)")
}

func TestWaitForMemoryEndsWithTheReply(t *testing.T) {
	p := &shared{meta: hugeFile}
	s := settings(t, hugeLimit)
	defer holding(t, p, s)()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	var log strings.Builder
	c := files.Ingest(ctx, p, s, slog.New(slog.NewTextHandler(&log, nil)), []string{"f1"}, analysed("second"))
	check(t, c, "files.download_failed filename=huge.png", "part huge.png (png)")
	if n := p.downloads.Load(); n != 1 {
		t.Errorf("%d downloads, want only the first reply's", n)
	}
	if want := `file_id=f1 error="context deadline exceeded"`; !strings.Contains(log.String(), want) {
		t.Errorf("log %q lacks %q", log.String(), want)
	}
}
