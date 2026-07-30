package cln

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// makeRune builds a rune: 32 bytes of hash state, then the restriction string.
func makeRune(t *testing.T, restrictions string) string {
	t.Helper()
	body := append(make([]byte, runeHashBytes), []byte(restrictions)...)
	return base64.RawURLEncoding.EncodeToString(body)
}

// The rune the setup instructions tell people to make.
func TestARestrictedRuneReadsAsRestricted(t *testing.T) {
	t.Parallel()

	r := ParseRune(makeRune(t, "method^list|method=getinfo"))
	if !r.Readable {
		t.Fatal("a well-formed rune could not be read")
	}
	if r.Unrestricted() {
		t.Error("a restricted rune was reported as unrestricted")
	}
	if !r.RestrictsToReads() {
		t.Errorf("a rune limited to list and getinfo was not recognised as read-only: %v",
			r.Restrictions)
	}
}

// A rune that permits everything is a real finding, and different from one we
// could not read.
func TestAnUnrestrictedRuneIsReportedAsSuch(t *testing.T) {
	t.Parallel()

	// `createrune` with no arguments produces exactly this: the hash, and
	// nothing after it.
	r := ParseRune(base64.RawURLEncoding.EncodeToString(make([]byte, runeHashBytes)))
	if !r.Readable {
		t.Fatal("a rune that is exactly its hash is a readable, unrestricted rune — " +
			"saying we could not read it is weaker and less useful")
	}
	if !r.Unrestricted() {
		t.Error("a rune with no restrictions was not reported as unrestricted")
	}
	if r.RestrictsToReads() {
		t.Error("a rune with no restrictions cannot restrict to reads")
	}
}

// The distinction this whole file exists for: "we could not tell" must never
// present as "it is restricted".
func TestAnUnreadableRuneMakesNoClaim(t *testing.T) {
	t.Parallel()

	for name, token := range map[string]string{
		"not base64": "!!! not a rune !!!",
		"too short":  base64.RawURLEncoding.EncodeToString(make([]byte, 8)),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := ParseRune(token)
			if r.Readable {
				t.Errorf("reported as readable, giving %v", r.Restrictions)
			}
			if r.Unrestricted() {
				t.Error("an unreadable rune claimed to be unrestricted")
			}
			if r.RestrictsToReads() {
				t.Error("an unreadable rune claimed to be restricted to reads")
			}
		})
	}
}

// A restriction the reader does not understand must not be read as safe.
func TestRestrictionsThatCannotBeVouchedFor(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"method^list":                true,
		"method=getinfo":             true,
		"method=listpeerchannels":    true,
		"method^list|method=getinfo": true,
		// A group whose alternatives include something that is not a read: any
		// alternative may satisfy the group, so the whole group fails.
		"method^list|method=pay": false,
		"method=pay":             false,
		// A prefix that is not "list" permits more than reading.
		"method^send": false,
		// Restrictions on something other than the method say nothing about
		// which methods are allowed.
		"time<1900000000":             false,
		"id=03aabb":                   false,
		"method^list&time<1900000000": true,
		// Comparisons this does not interpret.
		"method~list": false,
		"":            false,
	}
	for restrictions, want := range cases {
		r := ParseRune(makeRune(t, restrictions))
		if got := r.RestrictsToReads(); got != want {
			t.Errorf("RestrictsToReads(%q) = %v, want %v", restrictions, got, want)
		}
	}
}

// Runes are copied by hand and arrive in all four base64 spellings.
func TestRunesInEverySpelling(t *testing.T) {
	t.Parallel()

	body := append(make([]byte, runeHashBytes), []byte("method^list")...)
	for name, enc := range map[string]*base64.Encoding{
		"raw url":    base64.RawURLEncoding,
		"padded url": base64.URLEncoding,
		"raw std":    base64.RawStdEncoding,
		"padded std": base64.StdEncoding,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if r := ParseRune(enc.EncodeToString(body)); !r.Readable {
				t.Errorf("a rune in %s spelling could not be read", name)
			}
		})
	}
}

func TestLoadRune(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "forktower.rune")
	// With surrounding whitespace, as a file written by a shell redirect has.
	if err := os.WriteFile(path,
		[]byte("\n"+makeRune(t, "method^list")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r, err := LoadRune(path)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Readable || r.Token == "" {
		t.Errorf("got %+v", r)
	}

	if _, err := LoadRune(filepath.Join(dir, "not-there")); err == nil {
		t.Error("a missing credential file was accepted")
	}
	empty := filepath.Join(dir, "empty.rune")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRune(empty); err == nil {
		t.Error("an empty credential file was accepted")
	}
}
