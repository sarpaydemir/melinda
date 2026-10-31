package files

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"

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

// analysable are the media types and extensions the vision model reads; every text/ media type is read as well.
var (
	analysableTypes      = []string{"image/jpeg", "image/png", "image/webp", "application/pdf"}
	analysableExtensions = []string{
		".jpg", ".jpeg", ".png", ".webp", ".pdf",
		".doc", ".docx", ".xls", ".xlsx", ".xlsm", ".ppt", ".pptx", ".odt", ".ods", ".odp",
		".txt", ".md", ".csv", ".json", ".log", ".xml", ".yaml", ".yml",
	}
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
	maxMB := strconv.FormatInt(s.FileMaxBytes/1_000_000, 10)
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
			continue
		case !analysable(a.MediaType, ext):
			kind := cmp.Or(ext, a.MediaType, "application/octet-stream")
			c.note("files.unsupported", map[string]string{"filename": name, "kind": kind})
			continue
		case a.Size > s.FileMaxBytes:
			c.note("files.too_large", map[string]string{"filename": name, "max_mb": maxMB})
			continue
		}
		data, err := p.Download(ctx, id, s.FileMaxBytes)
		switch {
		case errors.Is(err, chat.ErrTooLarge):
			c.note("files.too_large", map[string]string{"filename": name, "max_mb": maxMB})
		case err != nil:
			log.WarnContext(ctx, "attachment download failed", "file_id", id, "error", err)
			c.note("files.download_failed", map[string]string{"filename": name})
		default:
			c.Notes = append(c.Notes, analyse(ctx, File{Name: name, Attachment: a, Data: data}))
		}
	}
	if s.VisionModel == "" {
		c.note("files.no_vision", nil)
	}
	return c
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
	return slices.Contains(analysableTypes, mediaType) || strings.HasPrefix(mediaType, "text/") ||
		slices.Contains(analysableExtensions, ext)
}
