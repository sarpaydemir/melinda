package files

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sarpaydemir/melinda/internal/textfmt"
)

const (
	textLimit      = 12000 // code points of a document's text kept
	truncated      = "... [truncated]"
	maxSheetValues = 200
	maxEntries     = 10000     // entries in one archive
	maxEntry       = 64 << 20  // decompressed bytes of one entry read
	maxExpanded    = 256 << 20 // decompressed bytes of all entries read
	maxToken       = 4 << 20   // bytes read for one tag or run of text
	maxDepth       = 10000     // nested elements
	// maxSharedStrings is as many as fit in one entry in the shortest form applications write.
	maxSharedStrings = maxEntry / len("<si><t/></si>")
	maxErrorText     = 200 // code points of an extraction error's text
	// extractionMemory is the most memory one extraction holds at once, measured on workbooks built to reach the
	// limits above.
	extractionMemory = 160 << 20
)

var errLimit = errors.New("document exceeds the extraction limits")

// officeText extracts the text of a .docx, .xlsx or .xlsm attachment, for when the vision model cannot read the
// file itself. Other files, and documents without text, give "".
func officeText(ctx context.Context, f File) (string, error) {
	ext := extension(f.Attachment)
	if !extractable(ext) {
		return "", nil
	}
	text, err := extract(ctx, ext, f.Data)
	if err != nil {
		return "", extractError{err}
	}
	return text, nil
}

func extractable(ext string) bool { return ext == ".docx" || ext == ".xlsx" || ext == ".xlsm" }

func extract(ctx context.Context, ext string, data []byte) (string, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	if len(r.File) > maxEntries {
		return "", errLimit
	}
	var out clipped
	if ext == ".docx" {
		err = docx(ctx, r.File, &out)
	} else {
		err = xlsx(ctx, r.File, &out)
	}
	if err != nil {
		return "", err
	}
	return out.String(), nil
}

// extractError keeps a failure's text short, since it can quote the document.
type extractError struct{ error }

func (e extractError) Error() string { return textfmt.Clip(e.error.Error(), maxErrorText) }

func (e extractError) Unwrap() error { return e.error }

// docx writes the text of the body and header parts in archive order, one line per part with text.
func docx(ctx context.Context, files []*zip.File, out *clipped) error {
	var parts []*zip.File
	for _, f := range files {
		if f.Name == "word/document.xml" || strings.HasPrefix(f.Name, "word/") && strings.HasSuffix(f.Name, ".xml") &&
			strings.Contains(f.Name, "header") {
			parts = append(parts, f)
		}
	}
	if err := fits(parts); err != nil {
		return err
	}
	lines := 0
	for _, f := range parts {
		if out.full() {
			break
		}
		nodes := 0
		err := parse(ctx, f, func(tok xml.Token, _ int) bool {
			run, ok := tok.(xml.CharData)
			if !ok {
				return true
			}
			text := textfmt.Strip(string(run))
			switch {
			case text == "":
				return true
			case nodes > 0:
				out.write(" ")
			case lines > 0:
				out.write("\n")
			}
			out.write(text)
			nodes++
			return !out.full()
		})
		if err != nil {
			return err
		}
		if nodes > 0 {
			lines++
		}
	}
	return nil
}

// xlsx writes a line per worksheet with values, the worksheets sorted by entry name.
func xlsx(ctx context.Context, files []*zip.File, out *clipped) error {
	var strs *zip.File
	var sheets []*zip.File
	for _, f := range files {
		switch {
		case f.Name == "xl/sharedStrings.xml":
			strs = f
		case strings.HasPrefix(f.Name, "xl/worksheets/") && strings.HasSuffix(f.Name, ".xml"):
			sheets = append(sheets, f)
		}
	}
	read := sheets
	if strs != nil {
		read = append([]*zip.File{strs}, sheets...)
	}
	if err := fits(read); err != nil {
		return err
	}
	var shared table
	if strs != nil {
		shared, _ = sharedStrings(ctx, strs) // unreadable shared strings count as none
	}
	slices.SortFunc(sheets, func(x, y *zip.File) int { return strings.Compare(x.Name, y.Name) })
	lines := 0
	for _, f := range sheets {
		if out.full() {
			break
		}
		values, err := sheetValues(ctx, f, shared)
		if err != nil {
			return err
		}
		if len(values) == 0 {
			continue
		}
		if lines > 0 {
			out.write("\n")
		}
		out.write(f.Name + ": ")
		for i, v := range values {
			if i > 0 {
				out.write(" | ")
			}
			out.write(v)
		}
		lines++
	}
	return nil
}

// fits checks the sizes the entries' headers declare against the extraction limits. Reading an entry fails past
// its declared size.
func fits(files []*zip.File) error {
	var total uint64
	for _, f := range files {
		if f.UncompressedSize64 > maxEntry {
			return fmt.Errorf("%s: %w", f.Name, errLimit)
		}
		total += f.UncompressedSize64
	}
	if total > maxExpanded {
		return errLimit
	}
	return nil
}

// table holds a workbook's shared strings in one string; entry i ends at ends[i].
type table struct {
	text string
	ends []uint32
}

// sharedStrings reads a workbook's shared strings: each child of the root element gives the text of its nodes,
// trimmed and joined with spaces.
func sharedStrings(ctx context.Context, f *zip.File) (table, error) {
	var b strings.Builder
	var ends []uint32
	start := 0
	err := parse(ctx, f, func(tok xml.Token, depth int) bool {
		switch tok := tok.(type) {
		case xml.StartElement:
			if depth == 2 {
				start = b.Len()
			}
		case xml.EndElement:
			if depth == 2 {
				ends = append(ends, uint32(b.Len()))
			}
		case xml.CharData:
			text := textfmt.Strip(string(tok))
			if depth < 2 || text == "" {
				break
			}
			if b.Len() > start {
				b.WriteByte(' ')
			}
			b.WriteString(text)
		}
		return len(ends) <= maxSharedStrings
	})
	switch {
	case err != nil:
		return table{}, err
	case len(ends) > maxSharedStrings:
		return table{}, fmt.Errorf("%s: %w", f.Name, errLimit)
	}
	return table{b.String(), ends}, nil
}

// lookup returns the entry an index names, a negative index counting from the end, or the index itself when it
// names none.
func (t table) lookup(index string) string {
	i := textfmt.Int(index, math.MinInt)
	if i < 0 {
		i += len(t.ends)
	}
	if i < 0 || i >= len(t.ends) {
		return index
	}
	var start uint32
	if i > 0 {
		start = t.ends[i-1]
	}
	return t.text[start:t.ends[i]]
}

// sheetValues returns the first non-empty values of a worksheet's cells, in document order. A cell's value is the
// text of its first v child, looked up in the shared strings when the cell's type is s, and trimmed; only as much
// of it is kept as the text can show.
func sheetValues(ctx context.Context, f *zip.File, shared table) ([]string, error) {
	type element struct{ cell, shared, valued bool }
	var open []element
	var values []string
	next, lookup := false, false // the next token is the text of a cell's first v child
	err := parse(ctx, f, func(tok xml.Token, _ int) bool {
		isValue, isShared := next, lookup
		next = false
		switch tok := tok.(type) {
		case xml.StartElement:
			if n := len(open) - 1; n >= 0 && open[n].cell && !open[n].valued && tok.Name.Local == "v" {
				open[n].valued = true
				next, lookup = true, open[n].shared
			}
			e := element{cell: tok.Name.Local == "c"}
			e.shared = e.cell && cellType(tok) == "s"
			open = append(open, e)
		case xml.EndElement:
			open = open[:len(open)-1]
		case xml.CharData:
			if !isValue {
				break
			}
			v := string(tok)
			if isShared {
				v = shared.lookup(v)
			}
			if v = textfmt.Strip(v); v != "" {
				values = append(values, textfmt.Clip(v, textLimit+1))
			}
		}
		return len(values) < maxSheetValues
	})
	return values, err
}

// cellType is a cell's unqualified t attribute.
func cellType(c xml.StartElement) string {
	for _, a := range c.Attr {
		if a.Name.Space == "" && a.Name.Local == "t" {
			return a.Value
		}
	}
	return ""
}

// parse streams an entry's XML the way an element tree sees it, until fn returns false. fn gets each start and end
// tag with its element's depth, 1 for the root, and the character data between two tags as one run with the depth
// of the element holding it; comments and processing instructions do not split a run.
func parse(ctx context.Context, f *zip.File, fn func(tok xml.Token, depth int) bool) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("%s: %w", f.Name, err)
	}
	defer rc.Close()
	in := &gate{ctx: ctx, r: rc}
	d := xml.NewDecoder(in)
	var run []byte
	depth, rooted := 0, false
	flush := func() bool {
		if len(run) == 0 {
			return true
		}
		more := fn(xml.CharData(run), depth)
		run = run[:0]
		return more
	}
	for {
		in.n = 0
		tok, err := d.Token()
		switch {
		case err == io.EOF && !rooted:
			return fmt.Errorf("%s: no root element", f.Name)
		case err == io.EOF:
			return nil
		case err != nil:
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		switch tok := tok.(type) {
		case xml.CharData:
			if depth == 0 {
				break
			}
			if len(run)+len(tok) > maxToken {
				return fmt.Errorf("%s: %w", f.Name, errLimit)
			}
			run = append(run, tok...)
		case xml.StartElement:
			if !flush() {
				return nil
			}
			rooted = true
			if depth++; depth > maxDepth {
				return fmt.Errorf("%s: %w", f.Name, errLimit)
			}
			if !fn(tok, depth) {
				return nil
			}
		case xml.EndElement:
			if !flush() || !fn(tok, depth) {
				return nil
			}
			depth--
		}
	}
}

// gate fails a read once the context ends, or once more than maxToken bytes were read since n was reset, which
// bounds the memory the decoder takes for one token.
type gate struct {
	ctx context.Context
	r   io.Reader
	n   int
}

func (g *gate) Read(p []byte) (int, error) {
	if err := g.ctx.Err(); err != nil {
		return 0, err
	}
	if g.n > maxToken {
		return 0, errLimit
	}
	n, err := g.r.Read(p)
	g.n += n
	return n, err
}

// clipped keeps the first textLimit code points written to it, and one more to tell that the text was truncated.
type clipped struct {
	b strings.Builder
	n int
}

func (c *clipped) full() bool { return c.n > textLimit }

func (c *clipped) write(s string) {
	if c.full() {
		return
	}
	s = textfmt.Clip(s, textLimit+1-c.n)
	c.n += utf8.RuneCountInString(s)
	c.b.WriteString(s)
}

func (c *clipped) String() string {
	if c.full() {
		return textfmt.Clip(c.b.String(), textLimit) + truncated
	}
	return c.b.String()
}
