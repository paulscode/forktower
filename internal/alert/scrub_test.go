package alert

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The failure this exists for: a delivery error lands in the database, is
// returned by the API, and is then included in the support bundle users are
// invited to email to a maintainer. Anything secret in it has been handed over.
func TestScrubErrorRemovesCredentials(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		in      string
		secrets []string
		keep    string
	}{
		{
			name:    "credentials embedded in a url",
			in:      `Post "https://alice:s3cr3t@hooks.example.com/x": dial tcp: timeout`,
			secrets: []string{"alice", "s3cr3t"},
			keep:    "hooks.example.com",
		},
		{
			name:    "a token in the query string",
			in:      `Post "https://ntfy.example.com/topic?auth=tk_AgQWabcdef": 401`,
			secrets: []string{"tk_AgQWabcdef", "auth="},
			keep:    "ntfy.example.com",
		},
		{
			// An ntfy topic name is a bearer secret: anyone who knows it can read
			// the user's alerts, and people habitually choose guessable ones.
			name:    "a topic name in the url path",
			in:      `Post "https://ntfy.sh/pauls-private-forktower-alerts": 500`,
			secrets: []string{"pauls-private-forktower-alerts"},
			keep:    "ntfy.sh",
		},
		{
			name:    "an echoed authorization header",
			in:      "the webhook replied 401 Unauthorized: Authorization: Bearer eyJhbGciOi.J9",
			secrets: []string{"eyJhbGciOi.J9"},
			keep:    "401",
		},
		{
			name:    "a bare bearer token",
			in:      "rejected credential Bearer abc123def",
			secrets: []string{"abc123def"},
			keep:    "rejected credential",
		},
		{
			name:    "a labelled api key",
			in:      "request failed, x-api-key=9f8e7d6c5b4a sent",
			secrets: []string{"9f8e7d6c5b4a"},
			keep:    "request failed",
		},
		{
			name:    "a password in a connection string",
			in:      "smtp auth failed: password=hunter2",
			secrets: []string{"hunter2"},
			keep:    "smtp auth failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := scrubError(errors.New(tc.in))

			for _, secret := range tc.secrets {
				if strings.Contains(got, secret) {
					t.Errorf("%q survived scrubbing:\n  in:  %s\n  out: %s", secret, tc.in, got)
				}
			}
			// Scrubbing that removed everything would make the column useless and
			// push people towards reading logs that are not scrubbed at all.
			if tc.keep != "" && !strings.Contains(got, tc.keep) {
				t.Errorf("scrubbing removed %q, which is what makes the error actionable:\n  out: %s",
					tc.keep, got)
			}
		})
	}
}

func TestScrubErrorOnSuccess(t *testing.T) {
	t.Parallel()
	if got := scrubError(nil); got != "" {
		t.Errorf("a successful delivery scrubbed to %q, want the empty string", got)
	}
}

// A misconfigured endpoint can answer with a whole HTML page. Storing it would
// bloat a database that is read back by the API and exported in diagnostics.
func TestScrubErrorTruncates(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", MaxErrorLen*3)
	got := scrubError(fmt.Errorf("the webhook replied 500: %s", long))

	if len(got) > MaxErrorLen+len("…") {
		t.Errorf("scrubbed error is %d bytes, want at most %d", len(got), MaxErrorLen)
	}
	if !strings.HasPrefix(got, "the webhook replied 500") {
		t.Errorf("truncation dropped the useful part: %q", got)
	}
}

// Truncating inside a multi-byte character puts a broken string in the database,
// which is a second problem to debug on top of the one being reported.
func TestScrubErrorTruncatesOnRuneBoundaries(t *testing.T) {
	t.Parallel()

	got := scrubError(errors.New(strings.Repeat("é", MaxErrorLen)))
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected a truncated string, got %q", got)
	}
	for i, r := range got {
		if r == '�' {
			t.Errorf("truncation produced an invalid rune at byte %d: %q", i, got)
		}
	}
}

// A plain error with nothing sensitive in it must come through readable, or the
// scrubber makes every unrelated failure harder to diagnose.
func TestScrubErrorLeavesOrdinaryMessagesAlone(t *testing.T) {
	t.Parallel()

	const in = "connection refused"
	if got := scrubError(errors.New(in)); got != in {
		t.Errorf("got %q, want %q unchanged", got, in)
	}
}

func TestRedactUrlKeepsWhatIsUseful(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		// No path to hide, so nothing is invented.
		"https://ntfy.sh":                    "https://ntfy.sh",
		"https://ntfy.sh/":                   "https://ntfy.sh/" + Redacted,
		"https://ntfy.sh/topic":              "https://ntfy.sh/" + Redacted,
		"https://ntfy.sh?a=b":                "https://ntfy.sh/" + Redacted,
		"https://u:p@ntfy.sh:8443/topic?a=b": "https://ntfy.sh:8443/" + Redacted,
		"http://localhost:8080/hook":         "http://localhost:8080/" + Redacted,
		"https://ntfy.sh/a#frag":             "https://ntfy.sh/" + Redacted,
		"mailto://x":                         "mailto://x",
	}

	for in, want := range cases {
		if got := scrubString(in); got != want {
			t.Errorf("scrubString(%q) = %q, want %q", in, got, want)
		}
	}
}
