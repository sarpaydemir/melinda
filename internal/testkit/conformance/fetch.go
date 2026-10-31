package conformance

import (
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/sarpaydemir/dost/internal/port/web"
	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/webfake"
)

// maxPage is how much of a page body a fetcher reads.
const maxPage = 5 << 20

// Fetch checks a page fetch adapter against pages served on a loopback address, which is not public. open builds
// the adapter, configured to fetch non-public addresses when allowPrivate is true.
func Fetch(t *testing.T, open func(t *testing.T, allowPrivate bool) web.Fetcher) {
	// capped is a page whose first maxPage bytes end with tail.
	capped := func(tail string) string {
		head := "<p>Start</p><!--"
		return head + strings.Repeat("-", maxPage-len(head)-len("-->")-len(tail)) + "-->" + tail
	}
	site := webfake.New(t, map[string]testkit.Response{
		"/report":  webfake.HTML("<html><head><title>Quarterly report</title></head><body><p>Revenue grew.</p></body></html>"),
		"/fits":    webfake.HTML(capped("<p>End</p>")),
		"/exceeds": webfake.HTML(capped("") + "<p>End</p>"),
		"/missing": testkit.Status(http.StatusNotFound),
	})

	t.Run("PageTextHasTitleAndBody", func(t *testing.T) {
		text, err := open(t, true).Fetch(t.Context(), site.URL()+"/report")
		if err != nil || !strings.Contains(text, "Quarterly report") || !strings.Contains(text, "Revenue grew.") {
			t.Errorf("Fetch = %q, %v; want the title and the body text", text, err)
		}
	})

	t.Run("NonPublicAddressIsRefusedUnlessAllowed", func(t *testing.T) {
		before := len(site.Requests())
		text, err := open(t, false).Fetch(t.Context(), site.URL()+"/report")
		if err == nil || !strings.HasPrefix(err.Error(), "Error:") {
			t.Errorf("Fetch = %q, %v; want an error for the model", text, err)
		}
		if n := len(site.Requests()) - before; n != 0 {
			t.Errorf("the server received %d requests", n)
		}
	})

	t.Run("BodyIsReadUpToFiveMiB", func(t *testing.T) {
		f := open(t, true)
		for path, end := range map[string]bool{"/fits": true, "/exceeds": false} {
			text, err := f.Fetch(t.Context(), site.URL()+path)
			if err != nil || !strings.Contains(text, "Start") || strings.Contains(text, "End") != end {
				t.Errorf("%s: Fetch = %q, %v; want Start, and End %t", path, text, err, end)
			}
		}
	})

	t.Run("FailuresAreErrorsForTheModel", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		refused := "http://" + l.Addr().String() + "/report"
		l.Close()
		f := open(t, true)
		for _, url := range []string{site.URL() + "/missing", refused} {
			if text, err := f.Fetch(t.Context(), url); err == nil || !strings.HasPrefix(err.Error(), "Error:") {
				t.Errorf("Fetch(%s) = %q, %v; want an error starting with Error:", url, text, err)
			}
		}
	})
}
