package files

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sync/semaphore"

	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/chat"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

// Context is what a message's attachments contribute to a reply, in file order.
type Context struct {
	// Notes tell the model about the attachments, analyses included.
	Notes []Note
	// Parts summarize every attachment whose metadata was read.
	Parts []Part
}

// Note is a catalog entry with its placeholder values.
type Note struct {
	ID     string
	Values map[string]string
}

// File is a downloaded attachment, alive only while it is analysed.
type File struct {
	// Name is the attachment's name, or file_<id> when it has none.
	Name       string
	Attachment chat.Attachment
	Data       []byte
}

// Part names an attachment in the summary. Kind is its extension without the dot; when it is empty the summary
// shows the catalog's files.kind_fallback.
type Part struct {
	Name string
	Kind string
}

// mediaTypes are the extensions the vision model reads that imply a media type.
var mediaTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".pdf":  "application/pdf",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".xlsm": "application/vnd.ms-excel.sheet.macroEnabled.12",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".odt":  "application/vnd.oasis.opendocument.text",
	".ods":  "application/vnd.oasis.opendocument.spreadsheet",
	".odp":  "application/vnd.oasis.opendocument.presentation",
}

// The vision model also reads these media types, every text/ media type, and these extensions.
var (
	analysableTypes = []string{"image/jpeg", "image/png", "image/webp", "application/pdf"}
	textExtensions  = []string{".txt", ".md", ".csv", ".json", ".log", ".xml", ".yaml", ".yml"}
)

// Ingest reads the metadata of a message's attachments, in order and within the settings' file count, and
// downloads those the vision model can analyse within the settings' size limit, passing each to analyse before
// the next lookup. Without a vision model it only summarizes them. Failed lookups and downloads become notes;
// their errors go to log.
func Ingest(ctx context.Context, p chat.Platform, s *config.Settings, log *slog.Logger, ids []string,
	analyse func(context.Context, File) Note) Context {
	var c Context
	if len(ids) == 0 {
		return c
	}
	if extra := len(ids) - s.FileMaxCount; extra > 0 {
		c.note("files.too_many", map[string]string{"count": strconv.Itoa(extra)})
		ids = ids[:s.FileMaxCount]
	}
	for _, id := range ids {
		a, err := p.Attachment(ctx, id)
		if err == nil && a == (chat.Attachment{}) {
			err = errors.New("empty metadata")
		}
		if err != nil {
			log.WarnContext(ctx, "attachment metadata unavailable", "file_id", id, "error", err)
			c.note("files.meta_failed", map[string]string{"file_id": id})
			continue
		}
		name := cmp.Or(a.Name, "file_"+id)
		ext := extension(a)
		c.Parts = append(c.Parts, Part{Name: name, Kind: strings.TrimLeft(ext, ".")})
		switch {
		case s.VisionModel == "":
		case !analysable(a.MediaType, ext):
			c.note("files.unsupported", map[string]string{"filename": name, "kind": cmp.Or(ext, mediaType(a))})
		case a.Size > s.FileMaxBytes:
			c.Notes = append(c.Notes, tooLarge(name, s))
		default:
			c.Notes = append(c.Notes, read(ctx, p, s, log, id, File{Name: name, Attachment: a}, analyse))
		}
	}
	if s.VisionModel == "" {
		c.note("files.no_vision", nil)
	}
	return c
}

// memoryBudget is the memory that attachments being analysed may hold at once, across all replies.
const memoryBudget = 256 << 20

// memory is what is left of the budget. An attachment holds its cost from before its download until its analysis
// returns.
var memory = semaphore.NewWeighted(memoryBudget)

// read downloads attachment id and passes it to analyse, holding the attachment's cost in the memory budget
// meanwhile. A download that fails or passes the size limit gives a note instead, and so does a wait for the budget
// that ctx ends.
func read(ctx context.Context, p chat.Platform, s *config.Settings, log *slog.Logger, id string, f File,
	analyse func(context.Context, File) Note) Note {
	size := f.Attachment.Size
	if size <= 0 { // not reported
		size = s.FileMaxBytes
	}
	n := cost(extension(f.Attachment), size)
	err := memory.Acquire(ctx, n)
	if err == nil {
		defer memory.Release(n)
		f.Data, err = p.Download(ctx, id, s.FileMaxBytes)
	}
	switch {
	case errors.Is(err, chat.ErrTooLarge):
		return tooLarge(f.Name, s)
	case err != nil:
		log.WarnContext(ctx, "attachment download failed", "file_id", id, "error", err)
		return Note{ID: "files.download_failed", Values: map[string]string{"filename": f.Name}}
	}
	return analyse(ctx, f)
}

// cost estimates the most memory that analysing an attachment of size bytes holds at once: three times its size and
// 1 MiB for the download and the model request that carries it in base64, and the extraction of a document's text.
// An attachment that needs more than the whole budget takes all of it.
func cost(ext string, size int64) int64 {
	n := 3*min(size, memoryBudget) + 1<<20
	if extractable(ext) {
		n += extractionMemory
	}
	return min(n, memoryBudget)
}

func tooLarge(name string, s *config.Settings) Note {
	return Note{ID: "files.too_large", Values: map[string]string{
		"filename": name, "max_mb": strconv.FormatInt(s.FileMaxBytes/1_000_000, 10),
	}}
}

func (c *Context) note(id string, values map[string]string) {
	c.Notes = append(c.Notes, Note{ID: id, Values: values})
}

// extension is the attachment's extension, lowercase with a leading dot: the reported one, else the part of the
// name after its last dot, else "".
func extension(a chat.Attachment) string {
	if a.Extension != "" {
		return "." + textfmt.Lower(strings.TrimLeft(a.Extension, "."))
	}
	if i := strings.LastIndexByte(a.Name, '.'); i >= 0 {
		return "." + textfmt.Lower(a.Name[i+1:])
	}
	return ""
}

func analysable(mediaType, ext string) bool {
	_, typed := mediaTypes[ext]
	return slices.Contains(analysableTypes, mediaType) || strings.HasPrefix(mediaType, "text/") || typed ||
		slices.Contains(textExtensions, ext)
}

// mediaType is the attachment's reported media type, else the one its extension implies, else
// application/octet-stream.
func mediaType(a chat.Attachment) string {
	if a.MediaType != "" {
		return a.MediaType
	}
	return cmp.Or(mediaTypes[extension(a)], "application/octet-stream")
}
