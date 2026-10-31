package files

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sarpaydemir/dost/internal/port/chat"
)

// zipped archives name and body pairs in order. An entry listed in declared is stored with that uncompressed size
// in its header instead of its real one.
func zipped(t *testing.T, declared map[string]uint64, pairs ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for i := 0; i < len(pairs); i += 2 {
		name, body := pairs[i], pairs[i+1]
		var err error
		var f interface{ Write([]byte) (int, error) }
		if size, ok := declared[name]; ok {
			f, err = w.CreateRaw(&zip.FileHeader{
				Name: name, Method: zip.Store, CompressedSize64: uint64(len(body)), UncompressedSize64: size,
			})
		} else {
			f, err = w.Create(name)
		}
		if err == nil {
			_, err = f.Write([]byte(body))
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

// nested is the text x inside n nested elements.
func nested(n int) string { return strings.Repeat("<a>", n) + "x" + strings.Repeat("</a>", n) }

func office(name string, data []byte) File {
	return File{Name: name, Attachment: chat.Attachment{Name: name}, Data: data}
}

func TestOfficeText(t *testing.T) {
	var cells, first200 strings.Builder
	for i := range 205 {
		fmt.Fprintf(&cells, "<c><v>%d</v></c>", i)
		if i > 0 && i < 200 {
			first200.WriteString(" | ")
		}
		if i < 200 {
			fmt.Fprint(&first200, i)
		}
	}
	for _, tc := range []struct {
		name    string
		meta    chat.Attachment
		entries []string
		want    string
	}{
		{
			"every text node trimmed and joined with spaces", chat.Attachment{Name: "Rapor.DOCX"},
			[]string{"word/document.xml", "<?xml version=\"1.0\" encoding=\"UTF-8\" standalone=\"yes\"?>\r\n" +
				`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p>` +
				`<w:r><w:t xml:space="preserve"> Merhaba </w:t></w:r><w:r><w:t>dünya</w:t></w:r></w:p>` + "\n  " +
				"<w:p><w:r><w:t>İkinci\nsatır</w:t></w:r><w:r><w:instrText> PAGE </w:instrText></w:r></w:p>" +
				"</w:body></w:document>"},
			"Merhaba dünya İkinci\nsatır PAGE",
		},
		{
			"body and header parts in archive order, one line each, footers left out", chat.Attachment{Name: "a.docx"},
			[]string{
				"word/header2.xml", `<w:hdr xmlns:w="x"><w:p><w:t>Üst bilgi</w:t></w:p></w:hdr>`,
				"word/footer1.xml", `<w:ftr xmlns:w="x"><w:p><w:t>Alt bilgi</w:t></w:p></w:ftr>`,
				"word/document.xml", `<w:document xmlns:w="x"><w:p><w:t>Gövde</w:t></w:p></w:document>`,
				"word/header1.xml", `<w:hdr xmlns:w="x"><w:p><w:t>  </w:t></w:p></w:hdr>`,
				"word/_rels/header2.xml.rels", "<Relationships>rels</Relationships>",
				"word/glossary/document.xml", `<w:document xmlns:w="x">sözlük</w:document>`,
				"docProps/header.xml", "<a>dış</a>",
				"word/media/headerImage.xml", "<a>ek <b>başlık</b> kuyruk</a>",
			},
			"Üst bilgi\nGövde\nek başlık kuyruk",
		},
		{
			"comments, instructions and CDATA do not split text", chat.Attachment{Name: "a.docx"},
			[]string{"word/document.xml", "<a>baş<!-- yorum -->ı<?pi x?>ndaki<![CDATA[ <b> ]]>metin<b/>son</a>"},
			"başındaki <b> metin son",
		},
		{
			"Unicode whitespace trimmed", chat.Attachment{Name: "a.docx"},
			[]string{"word/document.xml", "<a><t>\u00a0 iç \u2003</t><t>\u3000\u2028</t><t>x\u00a0y</t></a>"},
			"iç x\u00a0y",
		},
		{
			"references decoded", chat.Attachment{Name: "a.docx"},
			[]string{"word/document.xml", "<a>A &amp; B &lt;C&gt; &#351;&#x131;</a>"},
			"A & B <C> şı",
		},
		{
			"byte order mark", chat.Attachment{Name: "a.docx"},
			[]string{"word/document.xml", "\ufeff<a>bom</a>"},
			"bom",
		},
		{
			"word document without text", chat.Attachment{Name: "a.docx"},
			[]string{"word/document.xml", `<w:document xmlns:w="x"><w:body><w:p/></w:body></w:document>`},
			"",
		},
		{
			"word document without body or headers", chat.Attachment{Name: "a.docx"},
			[]string{"word/styles.xml", "<a>stil</a>"},
			"",
		},
		{
			"numbers without shared strings", chat.Attachment{Name: "test.xlsx"},
			[]string{"xl/worksheets/sheet1.xml",
				`<worksheet xmlns="x"><sheetData><row><c><v>42</v></c></row></sheetData></worksheet>`},
			"xl/worksheets/sheet1.xml: 42",
		},
		{
			"cell values", chat.Attachment{Name: "a.xlsx"},
			[]string{
				"xl/sharedStrings.xml", `<sst xmlns="x" count="4"><si><t>Ürün</t></si><si><r><t>Ana</t></r>` +
					`<r><t xml:space="preserve"> fiyat </t></r><rPh><t>okunuş</t></rPh></si><si/><si><t> son </t></si></sst>`,
				"xl/worksheets/sheet1.xml", `<worksheet xmlns="x"><sheetData><row r="1">` +
					`<c r="A1" t="s"><v>0</v></c><c t="s"><v>1</v></c><c t="s"><v>2</v></c><c t="s"><v> 3 </v></c>` +
					`<c t="s"><v>-1</v></c><c t="s"><v>-4</v></c><c t="s"><v>-5</v></c><c t="s"><v>4</v></c>` +
					`<c t="s"><v>1.0</v></c><c t="s"><v>` + "\u0661" + `</v></c><c t="s"><v>0_1</v></c>` +
					`<c t="str"><f>A1</f><v>hesap</v></c><c t="inlineStr"><is><t>satır içi</t></is></c>` +
					`<c t="b"><v>1</v></c><c><v></v></c><c><v>  </v></c><c><f>SUM()</f></c><c><v> 7 </v></c>` +
					`<c><v>12<x/>34</v></c><c><v/><v>9</v></c><c><x><v>8</v></x></c><c><v>3.5</v></c>` +
					`</row></sheetData></worksheet>`,
			},
			"xl/worksheets/sheet1.xml: Ürün | Ana fiyat okunuş | son | son | Ürün | -5 | 4 | 1.0 | Ana fiyat okunuş | " +
				"Ana fiyat okunuş | hesap | 1 | 7 | 12 | 3.5",
		},
		{
			"cells in any namespace, typed only by an unqualified attribute", chat.Attachment{Name: "a.xlsm"},
			[]string{"xl/worksheets/sheet1.xml",
				`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:x="urn:other">` +
					`<x:c><x:v>1</x:v></x:c><c xmlns=""><v>2</v></c><c x:t="s"><v>3</v></c></worksheet>`},
			"xl/worksheets/sheet1.xml: 1 | 2 | 3",
		},
		{
			"unparseable shared strings leave indexes as they are", chat.Attachment{Name: "a.xlsx"},
			[]string{
				"xl/sharedStrings.xml", "<sst><si><t>a</t></si>",
				"xl/worksheets/sheet1.xml", `<w><c t="s"><v>0</v></c><c t="s"><v>1</v></c></w>`,
			},
			"xl/worksheets/sheet1.xml: 0 | 1",
		},
		{
			"worksheets sorted by name, those without values left out", chat.Attachment{Name: "a.xlsx"},
			[]string{
				"xl/worksheets/sheet10.xml", "<w><c><v>on</v></c></w>",
				"xl/worksheets/sheet2.xml", "<w><c><v>iki</v></c><c><v>2</v></c></w>",
				"xl/worksheets/sheet3.xml", "<w><c><f>x</f></c></w>",
				"xl/worksheets/sheet1.xml", "<w><c><v>bir</v></c></w>",
				"xl/worksheets/_rels/sheet1.xml.rels", "<r><c><v>rels</v></c></r>",
				"xl/worksheets/sub/a.xml", "<w><c><v>alt</v></c></w>",
				"xl/chartsheets/sheet1.xml", "<w><c><v>grafik</v></c></w>",
				"xl/Worksheets/sheet0.xml", "<w><c><v>büyük</v></c></w>",
			},
			"xl/worksheets/sheet1.xml: bir\nxl/worksheets/sheet10.xml: on\nxl/worksheets/sheet2.xml: iki | 2\n" +
				"xl/worksheets/sub/a.xml: alt",
		},
		{
			"cells at any depth", chat.Attachment{Name: "a.xlsx"},
			[]string{"xl/worksheets/sheet1.xml", "<c><v>kök</v><c><v>iç</v></c></c>"},
			"xl/worksheets/sheet1.xml: kök | iç",
		},
		{
			"first 200 values of a sheet", chat.Attachment{Name: "a.xlsx"},
			[]string{"xl/worksheets/sheet1.xml", "<w>" + cells.String() + "</w>"},
			"xl/worksheets/sheet1.xml: " + first200.String(),
		},
		{
			"spreadsheet without values", chat.Attachment{Name: "a.xlsx"},
			[]string{"xl/worksheets/sheet1.xml", "<w><c><f>x</f></c></w>"},
			"",
		},
		{
			"extension reported by the platform", chat.Attachment{Name: "kitap", Extension: "XLSM"},
			[]string{"xl/worksheets/sheet1.xml", "<w><c><v>5</v></c></w>"},
			"xl/worksheets/sheet1.xml: 5",
		},
		{
			"word document named as a spreadsheet", chat.Attachment{Name: "a.xlsx"},
			[]string{"word/document.xml", "<a>x</a>"},
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := officeText(t.Context(), File{Name: tc.meta.Name, Attachment: tc.meta, Data: zipped(t, nil, tc.entries...)})
			if err != nil || got != tc.want {
				t.Errorf("officeText = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestOfficeTextOfOtherFilesIsEmptyWithoutReadingThem(t *testing.T) {
	for _, name := range []string{"test.pdf", "a.doc", "a.pptx", "a.txt", "docx"} {
		if got, err := officeText(t.Context(), office(name, []byte("not a zip"))); got != "" || err != nil {
			t.Errorf("%s: officeText = %q, %v; want empty", name, got, err)
		}
	}
}

func TestOfficeTextKeepsTheFirst12000CodePoints(t *testing.T) {
	x := strings.Repeat("x", 5000)
	for _, tc := range []struct {
		name    string
		file    string
		entries []string
		want    string
	}{
		{"exactly the limit", "a.docx",
			[]string{"word/document.xml", "<a>" + strings.Repeat("ş", 12000) + "</a>"},
			strings.Repeat("ş", 12000)},
		{"one code point over", "a.docx",
			[]string{"word/document.xml", "<a>" + strings.Repeat("ş", 12001) + "</a>"},
			strings.Repeat("ş", 12000) + "... [truncated]"},
		{"separators count", "a.docx",
			[]string{"word/document.xml", "<a>" + strings.Repeat("a", 11999) + "</a>", "word/header1.xml", "<a>b</a>"},
			strings.Repeat("a", 11999) + "\n... [truncated]"},
		{"separators within the limit", "a.docx",
			[]string{"word/document.xml", "<a>" + strings.Repeat("a", 11998) + "</a>", "word/header1.xml", "<a>b</a>"},
			strings.Repeat("a", 11998) + "\nb"},
		{"repeated shared strings", "a.xlsx",
			[]string{
				"xl/sharedStrings.xml", "<sst><si><t>" + x + "</t></si></sst>",
				"xl/worksheets/sheet1.xml", "<w>" + strings.Repeat(`<c t="s"><v>0</v></c>`, 3) + "</w>",
			},
			("xl/worksheets/sheet1.xml: " + x + " | " + x + " | " + x)[:12000] + "... [truncated]"},
		{"what follows once the text is complete is not read", "a.docx",
			[]string{"word/document.xml", "<a><b>" + strings.Repeat("a", 12001) + "</b><c></a>",
				"word/header1.xml", "<a><b></a>"},
			strings.Repeat("a", 12000) + "... [truncated]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := officeText(t.Context(), office(tc.file, zipped(t, nil, tc.entries...)))
			if err != nil || got != tc.want {
				t.Errorf("officeText = %.40q... (%d bytes), %v; want %.40q... (%d bytes)", got, len(got), err, tc.want,
					len(tc.want))
			}
		})
	}
}

func TestOfficeTextStopsReadingAWorksheetAtItsLastValue(t *testing.T) {
	data := zipped(t, nil, "xl/worksheets/sheet1.xml", "<w>"+strings.Repeat("<c><v>7</v></c>", 200)+"<c><v>8</v></c><c>")
	got, err := officeText(t.Context(), office("a.xlsx", data))
	if want := "xl/worksheets/sheet1.xml: " + strings.Repeat("7 | ", 199) + "7"; err != nil || got != want {
		t.Errorf("officeText = %.40q, %v; want %.40q", got, err, want)
	}
}

func TestOfficeTextFailsOnUnreadableDocuments(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		data []byte
	}{
		{"not an archive", "test.docx", []byte("broken data")},
		{"mismatched tags in a header", "a.docx",
			zipped(t, nil, "word/document.xml", "<a>x</a>", "word/header1.xml", "<a><b></a>")},
		{"unclosed element", "a.docx", zipped(t, nil, "word/document.xml", "<a>x")},
		{"empty part", "a.docx", zipped(t, nil, "word/document.xml", "<a>x</a>", "word/header1.xml", "")},
		{"whitespace-only part", "a.docx", zipped(t, nil, "word/document.xml", "<a>x</a>", "word/header1.xml", "   ")},
		{"declaration only", "a.docx",
			zipped(t, nil, "word/document.xml", "<a>x</a>", "word/header1.xml", "<?xml version='1.0'?>")},
		{"malformed worksheet", "a.xlsx",
			zipped(t, nil, "xl/worksheets/sheet1.xml", "<w><c><v>1</v></c></w>", "xl/worksheets/sheet2.xml", "<w><c>")},
		{"entry longer than its header says", "a.docx",
			zipped(t, map[string]uint64{"word/document.xml": 4}, "word/document.xml", "<a>xx</a>")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := officeText(t.Context(), office(tc.file, tc.data)); err == nil || errors.Is(err, errLimit) {
				t.Errorf("officeText = %q, %v; want a read or parse error", got, err)
			}
		})
	}
}

func TestOfficeTextErrorsQuoteLittleOfTheDocument(t *testing.T) {
	long := strings.Repeat("a", 60000)
	data := zipped(t, nil, "word/"+long+"header.xml", "<"+long+"></b"+long+">")
	if _, err := officeText(t.Context(), office("a.docx", data)); err == nil ||
		utf8.RuneCountInString(err.Error()) > maxErrorText {
		t.Errorf("officeText error has %d code points, want at most %d", utf8.RuneCountInString(fmt.Sprint(err)),
			maxErrorText)
	}
}

// entries is an archive holding a body part and n-1 other entries.
func entries(t *testing.T, n int) []byte {
	pairs := []string{"word/document.xml", "<a>x</a>"}
	for i := range n - 1 {
		pairs = append(pairs, fmt.Sprintf("customXml/item%d.xml", i), "")
	}
	return zipped(t, nil, pairs...)
}

func TestOfficeTextStopsAtTheExtractionLimits(t *testing.T) {
	parts := []string{"word/document.xml", "<a/>", "word/header1.xml", "<a/>", "word/header2.xml", "<a/>",
		"word/header3.xml", "<a/>"}
	fourEntries := map[string]uint64{
		"word/document.xml": maxEntry, "word/header1.xml": maxEntry, "word/header2.xml": maxEntry,
		"word/header3.xml": maxEntry,
	}
	half := strings.Repeat("x", maxToken/2)
	for _, tc := range []struct {
		name    string
		file    string
		data    []byte
		limited bool
	}{
		{"more than 10 000 entries", "a.docx", entries(t, maxEntries+1), true},
		{"part expanding beyond 64 MiB", "a.docx",
			zipped(t, map[string]uint64{"word/document.xml": maxEntry + 1}, "word/document.xml", "<a/>"), true},
		{"part expanding to 64 MiB", "a.docx",
			zipped(t, map[string]uint64{"word/document.xml": maxEntry}, "word/document.xml", "<a/>"), false},
		{"parts expanding beyond 256 MiB together", "a.docx",
			zipped(t, fourEntries, append(parts, "word/header4.xml", "<a/>")...), true},
		{"parts expanding to 256 MiB together", "a.docx", zipped(t, fourEntries, parts...), false},
		{"shared strings expanding beyond 64 MiB", "a.xlsx",
			zipped(t, map[string]uint64{"xl/sharedStrings.xml": maxEntry + 1},
				"xl/sharedStrings.xml", "<sst/>", "xl/worksheets/sheet1.xml", `<w><c t="s"><v>0</v></c></w>`), true},
		{"elements nested too deep", "a.docx", zipped(t, nil, "word/document.xml", nested(maxDepth+1)), true},
		{"element with too many attributes", "a.docx", zipped(t, nil,
			"word/document.xml", "<a"+strings.Repeat(` b=""`, (maxToken+8192)/5)+"/>"), true},
		{"run of text split by a comment", "a.docx", zipped(t, nil,
			"word/document.xml", "<a>"+half+"<!---->"+half+"x</a>"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := officeText(t.Context(), office(tc.file, tc.data)); err == nil ||
				errors.Is(err, errLimit) != tc.limited {
				t.Errorf("officeText = %q, %v; want an error, the limit error: %t", got, err, tc.limited)
			}
		})
	}
}

func TestOfficeTextReadsLargeDocumentsWithinTheLimits(t *testing.T) {
	text := strings.Repeat("y", maxToken-8192)
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"10 000 entries", entries(t, maxEntries), "x"},
		{"deep nesting", zipped(t, nil, "word/document.xml", nested(maxDepth)), "x"},
		{"long text", zipped(t, nil, "word/document.xml", "<a>"+text+"</a>"), text[:12000] + "... [truncated]"},
		{"large entries that are not read",
			zipped(t, map[string]uint64{"word/footer1.xml": maxExpanded + 1, "word/media/image1.png": maxEntry + 1},
				"word/document.xml", "<a>x</a>", "word/footer1.xml", "<a/>", "word/media/image1.png", "png"),
			"x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := officeText(t.Context(), office("a.docx", tc.data))
			if err != nil || got != tc.want {
				t.Errorf("officeText = %.40q, %v; want %.40q", got, err, tc.want)
			}
		})
	}
}

func TestExtractionStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tc := range []struct{ ext, entry string }{
		{".docx", "word/document.xml"}, {".xlsx", "xl/worksheets/sheet1.xml"},
	} {
		data := zipped(t, nil, tc.entry, "<a><c><v>1</v></c></a>")
		if got, err := extract(ctx, tc.ext, data); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: extract = %q, %v; want the context's error", tc.entry, got, err)
		}
	}
}

func TestOfficeTextWaitsForTheRunningExtraction(t *testing.T) {
	data := zipped(t, nil, "word/document.xml", "<a>x</a>")
	extracting <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := officeText(ctx, office("a.docx", data))
	<-extracting
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("officeText while another runs = %v, want the context's error", err)
	}
	if got, err := officeText(t.Context(), office("a.docx", data)); got != "x" || err != nil {
		t.Errorf("officeText after it finished = %q, %v; want x", got, err)
	}
}

func FuzzOfficeText(f *testing.F) {
	f.Add("<a>x<b>y</b>z</a>", "<sst><si><t>a</t></si></sst>")
	f.Add(`<w><c t="s"><v>0</v></c><c><v>1</v></c></w>`, "<sst><si><r><t>a</t></r><r><t>b</t></r></si><si/></sst>")
	f.Fuzz(func(t *testing.T, part, shared string) {
		data := zipped(t, nil, "word/document.xml", part, "word/header1.xml", part,
			"xl/sharedStrings.xml", shared, "xl/worksheets/sheet1.xml", part)
		for _, name := range []string{"a.docx", "a.xlsx"} {
			got, err := officeText(t.Context(), office(name, data))
			if n := utf8.RuneCountInString(got); n > textLimit+len(truncated) || err != nil && got != "" {
				t.Errorf("%s: officeText = %d code points, %v", name, n, err)
			}
		}
	})
}
