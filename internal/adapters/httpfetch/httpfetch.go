package httpfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sarpaydemir/dost/internal/config"
	"github.com/sarpaydemir/dost/internal/port/web"
	"github.com/sarpaydemir/dost/internal/textfmt"
)

const (
	// phase bounds connecting, the TLS handshake, and waiting for the response headers of each request; the whole
	// fetch, redirects and body included, is bounded by four phases.
	phase        = 8 * time.Second
	maxRedirects = 20
	maxBody      = 5 << 20
	maxHeader    = 64 << 10
	// maxQuote is how much of what a server sent a failure text quotes, in code points.
	maxQuote = 200
)

// header is sent with every request.
var header = http.Header{
	"User-Agent":      {"Mozilla/5.0 (X11; Linux x86_64; rv:109.0) Gecko/20100101 Firefox/117.0"},
	"Accept":          {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
	"Accept-Language": {"en-US,en;q=0.9"},
}

// New builds a fetcher from one settings snapshot. It connects only to public addresses unless
// web_fetch_allow_private is true.
func New(s *config.Settings) (web.Fetcher, error) {
	var c struct {
		AllowPrivate bool `json:"web_fetch_allow_private"`
	}
	if err := s.Decode(&c); err != nil {
		return nil, err
	}
	d := &net.Dialer{}
	if !c.AllowPrivate {
		d.Control = guard(public)
	}
	return newFetcher(d, phase), nil
}

// newFetcher returns a fetcher that connects with d, bounding each phase by phase.
func newFetcher(d *net.Dialer, phase time.Duration) *fetcher {
	d.Timeout = phase
	return &fetcher{
		transport: &http.Transport{
			DialContext:            d.DialContext,
			TLSHandshakeTimeout:    phase,
			ResponseHeaderTimeout:  phase,
			MaxResponseHeaderBytes: maxHeader,
			DisableKeepAlives:      true,
		},
		timeout: 4 * phase,
	}
}

type fetcher struct {
	transport http.RoundTripper
	timeout   time.Duration
}

// failure is a fetch failure whose text is written for the model.
type failure string

func (f failure) Error() string { return string(f) }

var errBlocked = errors.New("non-public address")

// Fetch returns "Title: <title>\nURL: <url>\n\n<text>" for the page at pageURL, which is fetched over https unless
// it starts with http. The title has at most 8000 code points and the text 5000; a failure text quotes at most
// maxQuote code points of anything the server sent.
func (f *fetcher) Fetch(ctx context.Context, pageURL string) (string, error) {
	if !strings.HasPrefix(pageURL, "http") {
		pageURL = "https://" + pageURL
	}
	page, err := f.get(ctx, pageURL)
	if err != nil {
		return "", err
	}
	title, text := extract(page)
	if text == "" {
		return "", failure("Error: No content found at " + pageURL)
	}
	return "Title: " + title + "\nURL: " + pageURL + "\n\n" + text, nil
}

// get returns the decoded body of the page at pageURL, up to maxBody bytes of it.
func (f *fetcher) get(ctx context.Context, pageURL string) (string, error) {
	var connected atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GetConn: func(string) { connected.Store(false) },
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", failure("Error: InvalidURL: " + quote(cause(err).Error()))
	}
	if err := checkURL(req.URL); err != nil {
		return "", err
	}
	req.Header = header.Clone()
	jar, _ := cookiejar.New(nil) // fails only for options it is not given
	client := &http.Client{Transport: f.transport, Jar: jar, Timeout: f.timeout, CheckRedirect: checkRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return "", failed(err, pageURL, connected.Load())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", failure(fmt.Sprintf("Error: Status %d", resp.StatusCode))
	}
	mediaType, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if !textual(mediaType) {
		return "", failure("Error: Unsupported content type " + quote(mediaType) + " at " + pageURL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", failed(err, pageURL, true)
	}
	return decode(body, params["charset"]), nil
}

// checkURL returns the failure for a URL that is not an http or https URL with a host.
func checkURL(u *url.URL) error {
	switch {
	case u.Hostname() == "":
		return failure("Error: UnsupportedProtocol: Request URL is missing an 'http://' or 'https://' protocol.")
	case u.Scheme != "http" && u.Scheme != "https":
		return failure("Error: UnsupportedProtocol: Request URL has an unsupported protocol '" + quote(u.Scheme) + "://'.")
	}
	return nil
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return failure("Error: TooManyRedirects: Exceeded maximum allowed redirects.")
	}
	return checkURL(req.URL)
}

// failed is the failure for err, an error of a request for pageURL; connected reports whether the request had
// a connection when it failed.
func failed(err error, pageURL string, connected bool) error {
	var f failure
	var timeout interface{ Timeout() bool }
	switch {
	case errors.As(err, &f):
		return f
	case errors.Is(err, errBlocked):
		return failure("Error: Access to " + pageURL + " is blocked (non-public address)")
	case errors.As(err, &timeout) && timeout.Timeout():
		return failure("Error: Timeout accessing " + pageURL)
	case !connected:
		return failure("Error: Cannot connect to " + pageURL)
	}
	return failure("Error: RemoteProtocolError: " + quote(cause(err).Error()))
}

func quote(s string) string { return textfmt.Clip(s, maxQuote) }

// cause is err without the URL that a *url.Error adds.
func cause(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return ue.Err
	}
	return err
}

func textual(mediaType string) bool {
	switch mediaType {
	case "", "application/xhtml+xml", "application/xml", "application/json":
		return true
	}
	return strings.HasPrefix(mediaType, "text/")
}

// guard returns a dialer Control function that refuses to connect to an address permit rejects.
func guard(permit func(netip.Addr) bool) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		if ap, err := netip.ParseAddrPort(address); err != nil || !permit(ap.Addr()) {
			return errBlocked
		}
		return nil
	}
}

var (
	nat64     = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour = netip.MustParsePrefix("2002::/16")
	// special are ranges of addresses that are not public beyond those the netip predicates name: this network,
	// shared address space, IETF protocol assignments, benchmarking, reserved (with broadcast), IPv4-compatible,
	// IPv4-translated, local-use NAT64, Teredo, and site-local.
	special = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("::/96"), netip.MustParsePrefix("::ffff:0:0:0/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
		netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("fec0::/10"),
	}
)

// public reports whether a is a public unicast address: not loopback, private, link-local, unspecified, multicast,
// or in a special range, in IPv4, IPv6, or IPv4-mapped IPv6 form. A NAT64 or 6to4 address is public when the IPv4
// address it embeds is.
func public(a netip.Addr) bool {
	a = a.Unmap()
	b := a.As16()
	switch {
	case nat64.Contains(a):
		return public(netip.AddrFrom4([4]byte(b[12:16])))
	case sixToFour.Contains(a):
		return public(netip.AddrFrom4([4]byte(b[2:6])))
	}
	return a.IsGlobalUnicast() && !a.IsPrivate() && !slices.ContainsFunc(special, func(p netip.Prefix) bool { return p.Contains(a) })
}
