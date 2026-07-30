package cln

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Rune is a Core Lightning authorisation token, and what it turned out to
// restrict.
type Rune struct {
	// Token is what goes in the request header. Never logged, never put in an
	// error, never persisted anywhere but the file it came from.
	Token string
	// Restrictions are the alternatives the rune carries, one entry per
	// restriction group, in the order they appear.
	Restrictions []string
	// Readable is false when the restrictions could not be established. Callers
	// must treat that as unknown, never as restricted.
	Readable bool
}

// runeHashBytes is the length of the SHA-256 state a rune carries before its
// restrictions.
const runeHashBytes = 32

// LoadRune reads a rune from a file and inspects what it permits.
//
// Inspected by decoding, never by calling a method to see what is refused: the
// methods that would answer that question are the ones that change things.
func LoadRune(path string) (Rune, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // an operator-supplied credential path, by design
	if err != nil {
		return Rune{}, fmt.Errorf("reading the Lightning credential: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return Rune{}, errors.New("the Lightning credential file is empty")
	}
	return ParseRune(token), nil
}

// ParseRune decodes a rune's restrictions.
//
// A rune is base64url of a 32-byte SHA-256 state followed by its restriction
// string; restrictions are separated by `&`, and alternatives within one by `|`.
// Small enough to read directly, and reading it is the only way to answer "is
// this credential restricted" without asking the node to refuse something.
func ParseRune(token string) Rune {
	r := Rune{Token: token}

	decoded, err := decodeRune(token)
	if err != nil || len(decoded) < runeHashBytes {
		// Not a rune at all. Reported as unreadable, which is a weaker statement
		// than either of the two below and deliberately so.
		return r
	}

	body := strings.TrimSpace(string(decoded[runeHashBytes:]))
	if body == "" {
		// A rune that is exactly its hash carries no restrictions, and that is a
		// finding rather than a failure: `createrune` with no arguments produces
		// one, and it grants everything. Saying "we could not read your
		// credential" here would be weaker, and less useful, than saying "your
		// credential is unrestricted".
		r.Readable = true
		return r
	}

	for _, group := range strings.Split(body, "&") {
		if group = strings.TrimSpace(group); group != "" {
			r.Restrictions = append(r.Restrictions, group)
		}
	}
	r.Readable = true
	return r
}

// decodeRune accepts either base64url spelling, with or without padding, since
// runes are copied by hand and arrive in all four forms.
func decodeRune(token string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.StdEncoding,
	} {
		if decoded, err := enc.DecodeString(token); err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("not base64")
}

// Unrestricted reports whether this rune permits everything.
//
// Only ever true when the rune was read successfully and carries no
// restrictions. An unreadable rune returns false here and false from Readable,
// and the caller reports it as unknown — a credential we could not inspect must
// not be described as either safe or unsafe.
func (r Rune) Unrestricted() bool {
	return r.Readable && len(r.Restrictions) == 0
}

// RestrictsToReads reports whether every method this rune allows is one that
// only reads.
//
// Conservative: it returns true only when it can see a method restriction that
// is confined to reads. Anything it cannot interpret returns false, so the
// warning fires. Forktower only ever reads, and a credential that could do more
// is worth saying so about even when the reason is our own uncertainty.
func (r Rune) RestrictsToReads() bool {
	if !r.Readable || len(r.Restrictions) == 0 {
		return false
	}
	for _, group := range r.Restrictions {
		if methodRestrictionIsReadOnly(group) {
			return true
		}
	}
	return false
}

// readMethods are the Core Lightning methods Forktower calls. All read-only.
var readMethods = map[string]bool{
	"getinfo":            true,
	"listpeerchannels":   true,
	"listclosedchannels": true,
	"listpeers":          true,
	"listfunds":          true,
}

// methodRestrictionIsReadOnly reads one restriction group.
//
// A group is alternatives joined by `|`, any of which may satisfy it — so a
// group only confines the credential to reads when *every* alternative does.
func methodRestrictionIsReadOnly(group string) bool {
	alternatives := strings.Split(group, "|")
	sawMethod := false

	for _, alt := range alternatives {
		field, op, value, ok := splitAlternative(alt)
		if !ok || field != "method" {
			// A group mentioning anything other than the method says nothing
			// about which methods are allowed.
			return false
		}
		sawMethod = true

		switch op {
		case "=":
			if !readMethods[value] {
				return false
			}
		case "^":
			// A prefix. Only "list" is safe: every list* method reads.
			if !strings.HasPrefix(value, "list") {
				return false
			}
		default:
			// Some other comparison. Not understood, so not trusted.
			return false
		}
	}
	return sawMethod
}

// splitAlternative pulls apart `field<op>value`, e.g. `method=getinfo`.
func splitAlternative(alt string) (field, op, value string, ok bool) {
	alt = strings.TrimSpace(alt)
	for i := 0; i < len(alt); i++ {
		switch alt[i] {
		case '=', '^', '$', '~', '<', '>', '!', '/':
			if i == 0 {
				return "", "", "", false
			}
			return alt[:i], string(alt[i]), alt[i+1:], true
		}
	}
	return "", "", "", false
}
