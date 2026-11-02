package httpfetch

import (
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"

	"github.com/sarpaydemir/melinda/internal/textfmt"
)

const (
	maxText  = 5000
	maxTitle = 8000
	// renamed follows the name of each formatting tag the parser reads, and is dropped from the text read.
	renamed = "\x01"
)

var (
	// dropped are the elements removed with their content before a page is read.
	dropped = []string{"script", "style", "nav", "footer", "header", "iframe"}
	// unread are the elements whose text is not part of the page text.
	unread = []string{"template", "rt", "rp"}
	// formattingTag matches every start or end tag of a formatting element, in any context the tokenizer may read it.
	formattingTag = regexp.MustCompile(`(</?)([bB]|[bB][iI][gG]|[cC][oO][dD][eE]|[eE][mM]|[fF][oO][nN][tT]|[iI]|` +
		`[nN][oO][bB][rR]|[sS]|[sS][mM][aA][lL][lL]|[sS][tT][rR][iI][kK][eE]|[sS][tT][rR][oO][nN][gG]|[tT][tT]|[uU])` +
		`([\t\n\f\r />]|$)`)
)

// decode returns body as text in the named charset; UTF-8, with invalid bytes replaced, when the name is empty or
// unknown.
func decode(body []byte, name string) string {
	if enc, canonical := charset.Lookup(name); enc != nil && canonical != "utf-8" {
		if text, err := enc.NewDecoder().Bytes(body); err == nil {
			return string(text)
		}
	}
	return strings.ToValidUTF8(string(body), "\uFFFD")
}

// extract reads a page: the text of its first title element, clipped to maxTitle code points, and its text, one
// trimmed non-empty line per line of each text node, clipped to maxText code points. A byte order mark at the start
// and U+0001 are not text. A page the parser rejects, such as one nested too deeply, has no title and no text.
func extract(page string) (title, text string) {
	page = strings.TrimPrefix(page, "\uFEFF")
	title = "No Title"
	// Renamed, formatting elements are never reopened in later blocks, which can grow the tree far past the page.
	page = formattingTag.ReplaceAllString(page, "${1}${2}"+renamed+"$3")
	doc, err := html.ParseWithOptions(strings.NewReader(page), html.ParseOptionEnableScripting(false))
	if err != nil {
		return title, ""
	}
	found := false
	var lines []string
	var walk func(n *html.Node, read bool)
	walk = func(n *html.Node, read bool) {
		switch n.Type {
		case html.ElementNode:
			if slices.Contains(dropped, n.Data) {
				return
			}
			if n.Data == "title" && !found {
				title, found = strings.ReplaceAll(titleText(n), renamed, ""), true
			}
			read = read && !slices.Contains(unread, n.Data)
		case html.TextNode:
			if read {
				for line := range strings.SplitSeq(strings.ReplaceAll(n.Data, renamed, ""), "\n") {
					if line = textfmt.Strip(line); line != "" {
						lines = append(lines, line)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, read)
		}
	}
	walk(doc, true)
	return textfmt.Clip(title, maxTitle), textfmt.Clip(strings.Join(lines, "\n"), maxText)
}

// titleText is the text of a title element whose only child is text, or None.
func titleText(n *html.Node) string {
	if c := n.FirstChild; c != nil && c == n.LastChild && c.Type == html.TextNode {
		return c.Data
	}
	return "None"
}
