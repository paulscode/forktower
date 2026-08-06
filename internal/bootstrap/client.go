package bootstrap

import (
	"net"
	"net/http"
	"time"
)

// Client-side bounds. None of them cap the transfer itself, which legitimately
// runs for hours.
const (
	// responseHeaderTimeout bounds the wait for a server to begin answering.
	// Generous, because through Tor the first byte can be a long time coming.
	responseHeaderTimeout = 2 * time.Minute
	// tlsHandshakeTimeout bounds the handshake alone.
	tlsHandshakeTimeout = 60 * time.Second
	// idleConnTimeout closes pooled connections between parts.
	idleConnTimeout = 90 * time.Second
)

// NewHTTPClient builds the client the fetcher uses.
//
// **No overall timeout, deliberately.** An http.Client's Timeout covers the whole
// exchange including the body, so any value large enough for a two-gigabyte part
// over Tor would be far too large to detect anything, and any value small enough
// to be useful would kill a healthy download. The fetcher's stall watchdog does
// that job properly instead — it measures whether bytes are arriving, which is
// the actual question.
//
// A non-empty proxy routes everything through SOCKS5, hostname included. See
// socksDialer for why the hostname matters.
func NewHTTPClient(proxy string) *http.Client {
	transport := &http.Transport{
		ResponseHeaderTimeout: responseHeaderTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		IdleConnTimeout:       idleConnTimeout,
		ForceAttemptHTTP2:     true,
		// One at a time. This fetches a single large file sequentially, and a
		// connection pool sized for a browser would just hold sockets open
		// through a Tor daemon that has better uses for them.
		MaxIdleConns:        2,
		MaxIdleConnsPerHost: 2,
	}

	if proxy != "" {
		dialer := socksDialer{
			Address: proxy,
			Dialer:  &net.Dialer{Timeout: 30 * time.Second},
		}
		transport.DialContext = dialer.DialContext
		// **Turned off explicitly rather than left to chance.** Go's transport
		// otherwise consults the HTTP_PROXY environment variables, and a value
		// there would silently take precedence over the address the user
		// configured — sending a request meant for Tor somewhere else entirely.
		transport.Proxy = nil
	} else {
		transport.DialContext = (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
		transport.Proxy = nil
	}

	return &http.Client{
		Transport: transport,
		Timeout:   0,
		// Redirects are followed — the download host redirects to its storage
		// backend — but not indefinitely, and never back to a plain-text scheme
		// once TLS has been established.
		CheckRedirect: checkRedirect,
	}
}

// maxRedirects bounds a redirect chain. The real one is a single hop; anything
// beyond a handful is a loop.
const maxRedirects = 5

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return http.ErrUseLastResponse
	}
	// A downgrade from https to http would hand the request to whoever is on the
	// path. The content is verified either way, so this costs nothing to enforce
	// and removes a way for a network to see exactly which file is being asked
	// for.
	if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return errInsecureRedirect
	}
	return nil
}

// errInsecureRedirect is returned rather than followed.
var errInsecureRedirect = &redirectError{}

type redirectError struct{}

func (*redirectError) Error() string {
	return "bootstrap: refused a redirect from https to plain http"
}
