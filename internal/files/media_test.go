package files

import (
	"testing"

	"github.com/sarpaydemir/dost/internal/port/chat"
)

func TestMediaTypeIsTheReportedOneElseImpliedByTheExtension(t *testing.T) {
	for _, tc := range []struct {
		meta chat.Attachment
		want string
	}{
		{chat.Attachment{Name: "a.docx", MediaType: "text/plain"}, "text/plain"},
		{chat.Attachment{Name: "x.png", MediaType: "image/PNG"}, "image/PNG"},
		{chat.Attachment{Name: "x.png", MediaType: "application/zip"}, "application/zip"},
		{chat.Attachment{Name: "a.jpg"}, "image/jpeg"},
		{chat.Attachment{Name: "a.JPEG"}, "image/jpeg"},
		{chat.Attachment{Name: "a.png"}, "image/png"},
		{chat.Attachment{Name: "x", Extension: "PNG"}, "image/png"},
		{chat.Attachment{Name: "a.webp"}, "image/webp"},
		{chat.Attachment{Name: "a.pdf"}, "application/pdf"},
		{chat.Attachment{Name: "a.doc"}, "application/msword"},
		{chat.Attachment{Name: "a.docx"}, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{chat.Attachment{Name: "a.xls"}, "application/vnd.ms-excel"},
		{chat.Attachment{Name: "a.xlsx"}, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
		{chat.Attachment{Name: "a.xlsm"}, "application/vnd.ms-excel.sheet.macroEnabled.12"},
		{chat.Attachment{Name: "a.ppt"}, "application/vnd.ms-powerpoint"},
		{chat.Attachment{Name: "a.pptx"}, "application/vnd.openxmlformats-officedocument.presentationml.presentation"},
		{chat.Attachment{Name: "a.odt"}, "application/vnd.oasis.opendocument.text"},
		{chat.Attachment{Name: "a.ods"}, "application/vnd.oasis.opendocument.spreadsheet"},
		{chat.Attachment{Name: "a.odp"}, "application/vnd.oasis.opendocument.presentation"},
		{chat.Attachment{Name: "a.txt"}, "application/octet-stream"},
		{chat.Attachment{Name: "a.csv"}, "application/octet-stream"},
		{chat.Attachment{Name: "a.gif"}, "application/octet-stream"},
		{chat.Attachment{Name: "README"}, "application/octet-stream"},
	} {
		if got := mediaType(tc.meta); got != tc.want {
			t.Errorf("mediaType(%+v) = %q, want %q", tc.meta, got, tc.want)
		}
	}
}
