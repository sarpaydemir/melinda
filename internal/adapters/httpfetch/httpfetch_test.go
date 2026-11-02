package httpfetch

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/sarpaydemir/melinda/internal/config"
	"github.com/sarpaydemir/melinda/internal/port/web"
	"github.com/sarpaydemir/melinda/internal/testkit"
	"github.com/sarpaydemir/melinda/internal/testkit/conformance"
	"github.com/sarpaydemir/melinda/internal/testkit/webfake"
)

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

func open(t *testing.T, allowPrivate bool) web.Fetcher {
	t.Helper()
	f, err := New(settings(t, fmt.Sprintf(`{"model":"m","web_fetch_allow_private":%t}`, allowPrivate)))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// fetch returns the text of the page at url, or the error's text.
func fetch(f web.Fetcher, url string) string {
	text, err := f.Fetch(context.Background(), url)
	if err != nil {
		return err.Error()
	}
	return text
}

func TestFetchConformance(t *testing.T) {
	conformance.Fetch(t, open)
}

func TestNewRejectsAnAllowPrivateSettingThatIsNotABoolean(t *testing.T) {
	_, err := New(settings(t, `{"model":"m","web_fetch_allow_private":"yes"}`))
	if err == nil || !strings.Contains(err.Error(), "web_fetch_allow_private") {
		t.Errorf("New = %v, want an error naming web_fetch_allow_private", err)
	}
}

// TestPagesReadAsTheEstablishedText serves each page in testdata/pages and compares the result with the .want file
// next to it, where {url} stands for the page's URL. Misnested markup is repaired differently, which can split a
// line, so malformed pages are compared with runs of whitespace taken as one space.
func TestPagesReadAsTheEstablishedText(t *testing.T) {
	malformed := map[string]bool{"malformed": true}
	types := map[string]string{".html": "text/html; charset=utf-8", ".json": "application/json",
		".txt": "text/plain; charset=utf-8", ".xml": "application/xml"}
	pages, err := filepath.Glob(filepath.Join("testdata", "pages", "*.want"))
	if err != nil || len(pages) == 0 {
		t.Fatalf("no pages: %v", err)
	}
	site := testkit.NewServer(t)
	f := open(t, true)
	for _, want := range pages {
		name := strings.TrimSuffix(filepath.Base(want), ".want")
		t.Run(name, func(t *testing.T) {
			sources, _ := filepath.Glob(filepath.Join("testdata", "pages", name+".*"))
			var page string
			for _, s := range sources {
				if types[filepath.Ext(s)] != "" {
					page = s
				}
			}
			body, err := os.ReadFile(page)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := os.ReadFile(want)
			if err != nil {
				t.Fatal(err)
			}
			path := "/pages/" + filepath.Base(page)
			site.On("GET", path).Default(testkit.Raw(http.StatusOK, types[filepath.Ext(page)], body))
			url := site.URL() + path
			got, want := fetch(f, url), strings.ReplaceAll(string(expected), "{url}", url)
			if malformed[name] {
				got, want = strings.Join(strings.Fields(got), " "), strings.Join(strings.Fields(want), " ")
			}
			if got != want {
				t.Errorf("got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func TestSendsBrowserHeaders(t *testing.T) {
	site := webfake.New(t, map[string]testkit.Response{"/": webfake.HTML("<p>hi</p>")})
	fetch(open(t, true), site.URL()+"/")
	h := site.Requests()[0].Header
	for name, want := range map[string]string{
		"User-Agent":      "Mozilla/5.0 (X11; Linux x86_64; rv:109.0) Gecko/20100101 Firefox/117.0",
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "en-US,en;q=0.9",
	} {
		if got := h.Values(name); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestFailureTexts(t *testing.T) {
	truncated := testkit.Response{Handler: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "<p>short")
	}}
	site := webfake.New(t, map[string]testkit.Response{
		"/missing":    testkit.Status(http.StatusNotFound),
		"/no-content": testkit.Status(http.StatusNoContent),
		"/moved":      testkit.Status(http.StatusMovedPermanently),
		"/empty":      webfake.HTML(""),
		"/to-ftp":     webfake.Redirect("ftp://example.com/file", http.StatusFound),
		"/truncated":  truncated,
		// The parser refuses more than 512 open elements.
		"/deep":          webfake.HTML("<title>Deep</title><p>start</p>" + strings.Repeat("<div>", 600) + "end"),
		"/reconstructed": webfake.HTML("<p>" + strings.Repeat("<b id=x>", 600) + "x"),
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := l.Addr().String()
	l.Close()
	base, hostport := site.URL(), strings.TrimPrefix(site.URL(), "http://")
	missing := "Error: UnsupportedProtocol: Request URL is missing an 'http://' or 'https://' protocol."
	f := open(t, true)
	for _, c := range []struct{ url, want string }{
		{base + "/missing", "Error: Status 404"},
		{base + "/no-content", "Error: Status 204"},
		{base + "/moved", "Error: Status 301"},
		{base + "/empty", "Error: No content found at " + base + "/empty"},
		{base + "/to-ftp", "Error: UnsupportedProtocol: Request URL has an unsupported protocol 'ftp://'."},
		{base + "/truncated", "Error: RemoteProtocolError: unexpected EOF"},
		{base + "/deep", "Error: No content found at " + base + "/deep"},
		{base + "/reconstructed", "Error: No content found at " + base + "/reconstructed"},
		{"httpz://example.com", "Error: UnsupportedProtocol: Request URL has an unsupported protocol 'httpz://'."},
		{"http://", missing},
		{"https:///x", missing},
		{"http://:80/", missing},
		{"httpz:foo", missing},
		{"httpbin.org", missing},
		{"http://[::1", "Error: InvalidURL: missing ']' in host"},
		{"http://" + refused + "/", "Error: Cannot connect to http://" + refused + "/"},
		// Without an http prefix the URL is fetched over TLS, which the plain HTTP server cannot speak.
		{hostport + "/missing", "Error: Cannot connect to https://" + hostport + "/missing"},
	} {
		if got := fetch(f, c.url); got != c.want {
			t.Errorf("Fetch(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

func TestPagesAreReadOneAtATime(t *testing.T) {
	site := webfake.New(t, map[string]testkit.Response{"/": webfake.HTML("<title>T</title><p>x</p>")})
	url := site.URL() + "/"
	f := open(t, true)
	parsing <- struct{}{} // another page is being read
	done := make(chan string)
	go func() { done <- fetch(f, url) }()
	select {
	case got := <-done:
		t.Fatalf("Fetch returned %q while another page was read", got)
	case <-time.After(50 * time.Millisecond):
	}
	<-parsing
	if got, want := <-done, "Title: T\nURL: "+url+"\n\nT\nx"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWaitToReadAPageEndsWithTheReply(t *testing.T) {
	site := webfake.New(t, map[string]testkit.Response{"/": webfake.HTML("<title>T</title><p>x</p>")})
	url := site.URL() + "/"
	parsing <- struct{}{}
	defer func() { <-parsing }()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := open(t, true).Fetch(ctx, url); err == nil || err.Error() != "Error: Timeout accessing "+url {
		t.Errorf("Fetch = %v, want the timeout failure", err)
	}
	if n := len(site.Requests()); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

func TestANameThatDoesNotResolveCannotBeConnectedTo(t *testing.T) {
	f := newFetcher(&net.Dialer{Resolver: resolver(t)}, phase)
	if got, want := fetch(f, "http://nonexistent.invalid/"), "Error: Cannot connect to http://nonexistent.invalid/"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestServerSuppliedTextIsBounded(t *testing.T) {
	long := strings.Repeat("ş", 9000)
	site := webfake.New(t, map[string]testkit.Response{
		"/long-title": webfake.HTML("<title>" + long + "</title>"),
		"/long-type":  testkit.Raw(http.StatusOK, "image/"+strings.Repeat("p", 300), []byte("x")),
		"/long-header": {Header: http.Header{"Content-Type": {"text/html"}, "X-Padding": {strings.Repeat("v", 70<<10)}},
			Body: []byte("<p>x</p>")},
	})
	base := site.URL()
	f := open(t, true)
	for path, want := range map[string]string{
		"/long-title": "Title: " + long[:8000*len("ş")] + "\nURL: " + base + "/long-title\n\n" + long[:5000*len("ş")],
		"/long-type":  "Error: Unsupported content type image/" + strings.Repeat("p", 194) + " at " + base + "/long-type",
		"/long-header": "Error: RemoteProtocolError: net/http: HTTP/1.x transport connection broken: " +
			"net/http: server response headers exceeded 65536 bytes; aborted",
	} {
		if got := fetch(f, base+path); got != want {
			t.Errorf("%s: got %d code points %.80q, want %d code points %.80q", path, utf8.RuneCountInString(got), got,
				utf8.RuneCountInString(want), want)
		}
	}
}

func FuzzExtractBoundsTheTitleAndText(f *testing.F) {
	pages, _ := filepath.Glob(filepath.Join("testdata", "pages", "*.html"))
	for _, page := range pages {
		body, err := os.ReadFile(page)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(body)
	}
	for _, page := range []string{strings.Repeat("<div>", 513), "<p>" + strings.Repeat("<b id=x>", 600) + "x",
		"<table><a>x<tr>", "<svg><title><b>x</b></title>", "<frameset><p>after</p>", "<p>a\x00b</p>", "\xef\xbb\xbf<p>x"} {
		f.Add([]byte(page))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		title, text := extract(decode(body, ""))
		if utf8.RuneCountInString(title) > maxTitle || utf8.RuneCountInString(text) > maxText || !utf8.ValidString(text) {
			t.Errorf("extract = %d code points of title, %d of text (valid UTF-8: %t)", utf8.RuneCountInString(title),
				utf8.RuneCountInString(text), utf8.ValidString(text))
		}
	})
}

// TestFormattingLeftOpenAcrossManyBlocksIsReadInBoundedMemory covers pages that leave formatting elements open across
// many short blocks, where an HTML5 tree builder would reopen every one of them in every block.
func TestFormattingLeftOpenAcrossManyBlocksIsReadInBoundedMemory(t *testing.T) {
	var opened, anchors strings.Builder
	for i := range 100 {
		fmt.Fprintf(&opened, "<b a=%d>", i)
		fmt.Fprintf(&anchors, "<a a=%d>", i)
	}
	closed, blocks := strings.Repeat("</b>", 100), strings.Repeat("<p>x", 50<<10)
	for name, page := range map[string]string{
		"left open":                  "<p>" + opened.String() + blocks,
		"anchors left open":          "<p>" + anchors.String() + blocks,
		"closed inside a table":      "<p>" + opened.String() + "<table>" + closed + "</table>" + blocks,
		"closed inside a select":     "<p>" + opened.String() + "<select>" + closed + "</select>" + blocks,
		"opened inside noscript":     "<noscript><p>" + opened.String() + blocks,
		"opened inside an svg style": "<p><svg><style>" + opened.String() + "</style></svg>" + blocks,
		"closed after an svg end tag that is ignored": "<p>" + opened.String() + "<svg><foreignObject><div></svg>" +
			closed + "</div></foreignObject></svg></p>" + blocks,
		"closed after a table end tag that is ignored": "<p>" + opened.String() + "<table><template></table>" +
			closed + "</template></table></p>" + blocks,
		"closed after an svg title": "<p>" + opened.String() + "<svg><title></desc><span></svg>" + closed +
			"</span></title></svg></p>" + blocks,
	} {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, text := extract(page)
		runtime.ReadMemStats(&after)
		if want := strings.Repeat("x\n", maxText/2); text != want {
			t.Errorf("%s: text = %.20q (%d bytes), want %d lines of x", name, text, len(text), maxText/2)
		}
		if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 32<<20 {
			t.Errorf("%s: reading a %d KiB page allocated %d MiB", name, len(page)>>10, alloc>>20)
		}
	}
}

func TestAnchorsLeftOpenAroundManyBlocksAreRead(t *testing.T) {
	page := strings.Repeat("<a name=s>s<p>p</p>", 600)
	if _, text := extract(page); text != strings.TrimSuffix(strings.Repeat("s\np\n", 600), "\n") {
		t.Errorf("text = %.20q (%d bytes), want 600 pairs of s and p lines", text, len(text))
	}
}

func TestOnlyTextualContentTypesAreRead(t *testing.T) {
	site := webfake.New(t, map[string]testkit.Response{
		"/none":  {Header: http.Header{"Content-Type": nil}, Body: []byte("<p>no content type</p>")},
		"/json":  testkit.Raw(http.StatusOK, "application/json", []byte(`{"a": "<b>x</b>", "n": 1}`)),
		"/plain": testkit.Raw(http.StatusOK, "text/plain", []byte("line one\n\nline <two>\n  a < b > c\n")),
		"/xhtml": testkit.Raw(http.StatusOK, "application/xhtml+xml", []byte("<p>xhtml</p>")),
		"/pdf":   testkit.Raw(http.StatusOK, "application/pdf", []byte("%PDF-1.4 binary")),
		"/png":   testkit.Raw(http.StatusOK, "IMAGE/PNG; name=a.png", []byte("\x89PNG")),
	})
	base := site.URL()
	f := open(t, true)
	for path, want := range map[string]string{
		"/none":  "Title: No Title\nURL: " + base + "/none\n\nno content type",
		"/json":  "Title: No Title\nURL: " + base + "/json\n\n{\"a\": \"\nx\n\", \"n\": 1}",
		"/plain": "Title: No Title\nURL: " + base + "/plain\n\nline one\nline\na < b > c",
		"/xhtml": "Title: No Title\nURL: " + base + "/xhtml\n\nxhtml",
		"/pdf":   "Error: Unsupported content type application/pdf at " + base + "/pdf",
		"/png":   "Error: Unsupported content type image/png at " + base + "/png",
	} {
		if got := fetch(f, base+path); got != want {
			t.Errorf("%s: got %q, want %q", path, got, want)
		}
	}
}

func TestBodiesAreDecodedByTheCharsetOfTheContentType(t *testing.T) {
	turkish := []byte("<p>\xdeirket \xf0\xfc\xfd \xdd</p>") // "Şirket ğüı İ" in windows-1254 and ISO-8859-9
	site := webfake.New(t, map[string]testkit.Response{
		"/windows-1254": testkit.Raw(http.StatusOK, "text/html; charset=windows-1254", turkish),
		"/iso-8859-9":   testkit.Raw(http.StatusOK, "text/html; charset=ISO-8859-9", turkish),
		"/none":         testkit.Raw(http.StatusOK, "text/html", []byte("<p>caf\xe9</p>")),
		"/meta":         testkit.Raw(http.StatusOK, "text/html", []byte(`<meta charset="windows-1254"><p>`+"\xdeirket</p>")),
		"/unknown":      testkit.Raw(http.StatusOK, "text/html; charset=no-such-charset", []byte("<p>café</p>")),
		"/invalid":      testkit.Raw(http.StatusOK, "text/html; charset=utf-8", []byte("<p>a\xffb\xe2\x82c\xc3</p>")),
		"/bom":          testkit.Raw(http.StatusOK, "text/html", []byte("\xef\xbb\xbf<title>BOM</title><p>x</p>")),
	})
	base := site.URL()
	f := open(t, true)
	for path, text := range map[string]string{
		"/windows-1254": "Şirket ğüı İ",
		"/iso-8859-9":   "Şirket ğüı İ",
		"/none":         "caf\uFFFD",
		"/meta":         "\uFFFDirket",
		"/unknown":      "café",
		"/invalid":      "a\uFFFDb\uFFFDc\uFFFD",
	} {
		if got, want := fetch(f, base+path), "Title: No Title\nURL: "+base+path+"\n\n"+text; got != want {
			t.Errorf("%s: got %q, want %q", path, got, want)
		}
	}
	if got, want := fetch(f, base+"/bom"), "Title: BOM\nURL: "+base+"/bom\n\nBOM\nx"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRedirectsAreFollowedUpToTwenty(t *testing.T) {
	site := testkit.NewServer(t)
	site.On("GET", "/hop/{n}").DefaultFunc(func(r testkit.Request) testkit.Response {
		n, _ := strconv.Atoi(r.Params["n"])
		if n == 0 {
			return webfake.HTML("<title>Landed</title>")
		}
		return webfake.Redirect(fmt.Sprintf("/hop/%d", n-1), http.StatusFound)
	})
	f := open(t, true)
	for hops, want := range map[int]string{
		20: "Title: Landed\nURL: " + site.URL() + "/hop/20\n\nLanded",
		21: "Error: TooManyRedirects: Exceeded maximum allowed redirects.",
	} {
		if got := fetch(f, fmt.Sprintf("%s/hop/%d", site.URL(), hops)); got != want {
			t.Errorf("%d redirects: got %q, want %q", hops, got, want)
		}
	}
}

func TestCookiesSetDuringRedirectsAreSentBack(t *testing.T) {
	site := testkit.NewServer(t)
	site.On("GET", "/login").DefaultFunc(func(r testkit.Request) testkit.Response {
		if strings.Contains(r.Header.Get("Cookie"), "seen=1") {
			return webfake.HTML("<p>cookie ok</p>")
		}
		resp := webfake.Redirect("/login", http.StatusFound)
		resp.Header.Set("Set-Cookie", "seen=1; Path=/")
		return resp
	})
	want := "Title: No Title\nURL: " + site.URL() + "/login\n\ncookie ok"
	if got := fetch(open(t, true), site.URL()+"/login"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTimeoutsBoundEachPhaseAndTheWholeFetch(t *testing.T) {
	stalled := testkit.Response{Handler: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<p>first part")
		http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}}
	site := webfake.New(t, map[string]testkit.Response{
		"/slow-headers": webfake.HTML("<p>late</p>").WithDelay(time.Hour),
		"/stalled-body": stalled,
	})
	f := newFetcher(&net.Dialer{}, 50*time.Millisecond)
	for _, path := range []string{"/slow-headers", "/stalled-body"} {
		start := time.Now()
		if got, want := fetch(f, site.URL()+path), "Error: Timeout accessing "+site.URL()+path; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%s: returned after %v", path, d)
		}
	}
}

func TestPublicAddresses(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "93.184.215.14": true, "172.32.0.1": true, "100.63.255.255": true, "100.128.0.0": true,
		"2606:4700:4700::1111": true, "::ffff:8.8.8.8": true,
		"127.0.0.1": false, "127.255.255.254": false, "::1": false, "::ffff:127.0.0.1": false,
		"10.1.2.3": false, "172.16.0.1": false, "172.31.255.255": false, "192.168.1.1": false,
		"fc00::1": false, "fd00:ec2::254": false, "::ffff:10.0.0.1": false, "fe80::1": false,
		"169.254.169.254": false, "169.254.0.1": false, "::ffff:169.254.169.254": false, "fe80::1%eth0": false,
		"100.64.0.1": false, "100.127.255.254": false, "::ffff:100.100.100.200": false,
		"0.0.0.0": false, "::": false, "::ffff:0.0.0.0": false,
		"224.0.0.1": false, "239.255.255.250": false, "ff02::1": false, "ff0e::1": false,
		"255.255.255.255": false, "::ffff:255.255.255.255": false,
		"0.1.2.3": false, "192.0.0.192": false, "198.18.0.1": false, "198.19.255.255": false, "240.0.0.1": false,
		"192.0.1.1": true, "198.20.0.1": true, "223.255.255.254": true,
		"64:ff9b::a00:1": false, "64:ff9b::a9fe:a9fe": false, "64:ff9b::7f00:1": false, "64:ff9b::808:808": true,
		"64:ff9b:1::a00:1": false, "64:ff9b:1::808:808": false,
		"2002:a00:1::1": false, "2002:a9fe:a9fe::1": false, "2002:7f00:1::1": false, "2002:808:808::1": true,
		"2001:0:4136:e378:8000:63bf:3fff:fdd2": false, "::7f00:1": false, "::808:808": false,
		"::ffff:0:7f00:1": false, "::ffff:0:808:808": false, "fec0::1": false,
	} {
		if got := public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("public(%s) = %t, want %t", addr, got, want)
		}
	}
}

func TestNonPublicAddressesAreBlockedBeforeConnecting(t *testing.T) {
	site := webfake.New(t, map[string]testkit.Response{"/": webfake.HTML("<p>internal</p>")})
	port := site.URL()[strings.LastIndex(site.URL(), ":")+1:]
	f := open(t, false)
	for _, url := range []string{
		site.URL() + "/",
		"http://[::ffff:127.0.0.1]:" + port + "/",
		"http://localhost:" + port + "/",
		"http://0.0.0.0:" + port + "/",
		"http://169.254.169.254/latest/meta-data/",
		"http://[fd00:ec2::254]/latest/meta-data/",
	} {
		if got, want := fetch(f, url), "Error: Access to "+url+" is blocked (non-public address)"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if n := len(site.Requests()); n != 0 {
		t.Errorf("the server received %d requests", n)
	}
}

// loopbackAsPublic opens a fetcher whose check counts 127.0.0.1, where the test servers listen, as a public address,
// and which resolves names with r.
func loopbackAsPublic(r *net.Resolver) web.Fetcher {
	loopback := netip.MustParseAddr("127.0.0.1")
	return newFetcher(&net.Dialer{
		Control:  guard(func(a netip.Addr) bool { return a == loopback || public(a) }),
		Resolver: r,
	}, phase)
}

func TestRedirectsToNonPublicAddressesAreBlocked(t *testing.T) {
	site := webfake.New(t, map[string]testkit.Response{
		"/metadata": webfake.Redirect("http://169.254.169.254/latest/meta-data/", http.StatusFound),
		"/private":  webfake.Redirect("http://[::ffff:10.0.0.1]/admin", http.StatusMovedPermanently),
	})
	f := loopbackAsPublic(nil)
	for _, path := range []string{"/metadata", "/private"} {
		url := site.URL() + path
		if got, want := fetch(f, url), "Error: Access to "+url+" is blocked (non-public address)"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestANameIsCheckedAtTheAddressItResolvesToForEachConnection(t *testing.T) {
	site := webfake.New(t, map[string]testkit.Response{"/report": webfake.HTML("<p>public report</p>")})
	port := site.URL()[strings.LastIndex(site.URL(), ":")+1:]
	f := loopbackAsPublic(resolver(t, "127.0.0.1", "10.0.0.1"))
	url := "http://rebinding.test:" + port + "/report"
	for _, want := range []string{
		"Title: No Title\nURL: " + url + "\n\npublic report",
		"Error: Access to " + url + " is blocked (non-public address)",
	} {
		if got := fetch(f, url); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// resolver is a DNS resolver that answers A queries for any name with the next of addrs, the last one once the
// others are used, and AAAA queries with no address. Without addrs, it answers that no name exists.
func resolver(t *testing.T, addrs ...string) *net.Resolver {
	rcode := dnsmessage.RCodeSuccess
	if len(addrs) == 0 {
		rcode = dnsmessage.RCodeNameError
	}
	var mu sync.Mutex
	next := func() [4]byte {
		mu.Lock()
		defer mu.Unlock()
		a := netip.MustParseAddr(addrs[0])
		if len(addrs) > 1 {
			addrs = addrs[1:]
		}
		return a.As4()
	}
	answer := func(query []byte) ([]byte, error) {
		var p dnsmessage.Parser
		h, err := p.Start(query)
		if err != nil {
			return nil, err
		}
		q, err := p.Question()
		if err != nil {
			return nil, err
		}
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true, RCode: rcode})
		if err := b.StartQuestions(); err != nil {
			return nil, err
		}
		if err := b.Question(q); err != nil {
			return nil, err
		}
		if err := b.StartAnswers(); err != nil {
			return nil, err
		}
		if q.Type == dnsmessage.TypeA && rcode == dnsmessage.RCodeSuccess {
			rh := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET}
			if err := b.AResource(rh, dnsmessage.AResource{A: next()}); err != nil {
				return nil, err
			}
		}
		return b.Finish()
	}
	return &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			for {
				var n uint16
				if binary.Read(server, binary.BigEndian, &n) != nil {
					return
				}
				query := make([]byte, n)
				if _, err := io.ReadFull(server, query); err != nil {
					return
				}
				reply, err := answer(query)
				if err != nil {
					t.Errorf("fake DNS: %v", err)
					return
				}
				if binary.Write(server, binary.BigEndian, uint16(len(reply))) != nil || binary.Write(server, binary.BigEndian, reply) != nil {
					return
				}
			}
		}()
		return client, nil
	}}
}
