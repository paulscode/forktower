package alert

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxErrorLen bounds what is kept of a transport error.
//
// The column is read back by the API and travels in the support bundle users are
// invited to send to a maintainer, so a multi-kilobyte HTML error page from a
// misconfigured endpoint has no business being stored in full.
const MaxErrorLen = 300

// Redacted is what replaces anything removed, so a reader can tell the difference
// between a value that was withheld and one that was never there.
const Redacted = "[redacted]"

var (
	// urlRe matches a URL anywhere in a message. Go's own HTTP errors embed the
	// request URL — `Post "https://user:tok@host/topic?auth=x": dial tcp ...` —
	// so this is the main way a secret would reach the database.
	urlRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*)://([^\s"'` + "`" + `]*)`)

	// credRe matches a labelled credential and everything after it on the line.
	// Deliberately greedy to the end of the line: a token is not reliably one
	// whitespace-delimited word ("Bearer abc.def ghi"), and over-redacting an
	// error message costs a little context, while under-redacting one puts a
	// working credential in a file people email to strangers.
	credRe = regexp.MustCompile(`(?i)\b(authorization|proxy-authorization|x-api-key|api[-_]?key|apikey|token|password|passwd|secret)\b\s*[:=][^\r\n]*`)

	// bearerRe catches an unlabelled bearer token.
	bearerRe = regexp.MustCompile(`(?i)\bbearer\s+\S+`)
)

// scrubError renders an error safe to persist and to return over the API.
//
// Every transport error passes through here before it reaches
// `alert_deliveries.error`. HTTP and SMTP clients routinely echo the request URL,
// and a webhook or ntfy URL may carry a token in its userinfo, its query, or its
// path — an ntfy topic name *is* a bearer secret. A nil error scrubs to the empty
// string, so a successful delivery needs no special case at the call site.
func scrubError(err error) string {
	if err == nil {
		return ""
	}
	return scrubString(err.Error())
}

// scrubString is the same treatment for a message that is not an error, such as a
// status line a transport wants to record.
func scrubString(s string) string {
	s = urlRe.ReplaceAllStringFunc(s, redactURL)
	s = credRe.ReplaceAllString(s, "$1="+Redacted)
	s = bearerRe.ReplaceAllString(s, "bearer "+Redacted)

	s = strings.TrimSpace(s)
	if len(s) > MaxErrorLen {
		// Cut on a rune boundary; a half-written rune in the database is a second
		// problem to debug on top of the one being reported.
		cut := MaxErrorLen
		for cut > 0 && !isRuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// redactURL keeps the scheme and host and removes everything else.
//
// The host is what makes an error actionable — "cannot reach ntfy.sh" tells the
// user which service is down. The path and query are where the secrets are, and
// the transport's own name is recorded alongside the error anyway, so nothing is
// lost by dropping them.
func redactURL(match string) string {
	scheme, rest, ok := strings.Cut(match, "://")
	if !ok {
		return match
	}

	// Userinfo, if present, is credentials by definition.
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}

	host := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		host = rest[:i]
		return fmt.Sprintf("%s://%s/%s", scheme, host, Redacted)
	}
	return scheme + "://" + host
}
