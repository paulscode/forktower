// Package bitcoindview reads a chain from a Bitcoin Core or Knots node over
// JSON-RPC.
//
// It takes its own options rather than the daemon's configuration, so that
// nothing in this package depends on how the daemon happens to be configured and
// the whole chain layer stays importable on its own.
package bitcoindview

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Options describes one node to read from.
type Options struct {
	// RPCURL is the node's JSON-RPC endpoint, e.g. "http://127.0.0.1:8332".
	RPCURL string

	// CookiePath is the node's authentication cookie file. Either this or
	// User+Pass, not both.
	//
	// Preferred where available: the file is re-read when the node rejects a
	// request, so a node restart — which rewrites the cookie — does not silently
	// end the connection. A daemon that stops being able to see the chain and does
	// not notice is the failure mode this whole project exists to avoid, and
	// losing it to a routine restart would be an embarrassing way to hit it.
	CookiePath string

	// User and Pass authenticate when no cookie file is available.
	User string
	Pass string

	// Timeout bounds a single request. Zero uses DefaultTimeout.
	//
	// A ceiling, not a target: callers pass contexts with their own deadlines, and
	// the shorter of the two wins.
	Timeout time.Duration

	// HTTPClient overrides the client used. Zero value means one is built from
	// Timeout. Supplied by tests, and by anything needing a proxy.
	HTTPClient *http.Client

	// UserAgent identifies us in the node's logs, which is a courtesy to whoever
	// has to work out what is talking to their node.
	UserAgent string
}

// Defaults.
const (
	// DefaultTimeout bounds a single RPC call. Generous because one call here is a
	// local request that should take milliseconds; anything approaching this means
	// the node is in trouble, which is worth reporting rather than waiting out.
	DefaultTimeout = 30 * time.Second

	// DefaultUserAgent identifies this client to the node.
	DefaultUserAgent = "forktower"
)

// Validate checks the options are usable, reporting every problem at once.
func (o Options) Validate() error {
	var problems []string

	if o.RPCURL == "" {
		problems = append(problems, "rpc url is required")
	} else {
		u, err := url.Parse(o.RPCURL)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("rpc url is unparseable: %v", err))
		case u.Scheme != "http" && u.Scheme != "https":
			problems = append(problems, fmt.Sprintf("rpc url scheme %q must be http or https", u.Scheme))
		case u.Host == "":
			problems = append(problems, "rpc url has no host")
		}
	}

	hasCookie := o.CookiePath != ""
	hasUserPass := o.User != "" || o.Pass != ""
	switch {
	case hasCookie && hasUserPass:
		problems = append(problems, "set either a cookie path or a user and password, not both")
	case !hasCookie && !hasUserPass:
		problems = append(problems, "no authentication configured: set a cookie path or a user and password")
	case hasUserPass && (o.User == "" || o.Pass == ""):
		problems = append(problems, "authentication needs both a user and a password")
	}

	if o.Timeout < 0 {
		problems = append(problems, "timeout must not be negative")
	}

	if len(problems) > 0 {
		return fmt.Errorf("bitcoindview options: %s", joinProblems(problems))
	}
	return nil
}

func joinProblems(p []string) string {
	if len(p) == 1 {
		return p[0]
	}
	out := ""
	for i, s := range p {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}

// ErrNoAuth is returned when credentials cannot be obtained at all.
var ErrNoAuth = errors.New("bitcoindview: no usable credentials")

func (o Options) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return DefaultTimeout
}

func (o Options) userAgent() string {
	if o.UserAgent != "" {
		return o.UserAgent
	}
	return DefaultUserAgent
}
