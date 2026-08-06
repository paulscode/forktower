package bootstrap

import (
	"encoding/hex"
	"strings"
	"testing"
)

// The published manifest has to agree with itself, because nothing else checks
// it until nine gigabytes have moved.
//
// Every value here was transcribed by hand from a release. A digit dropped from a
// part length is not a compile error and not a test failure anywhere else — it is
// a download that runs to completion and fails its checksum, hours later, on
// somebody else's machine.
func TestThePublishedManifestIsSelfConsistent(t *testing.T) {
	s := MainnetHeight935000

	if got := s.TotalBytes(); got != 9_387_990_306 {
		t.Errorf("the parts add up to %d bytes, not the 9,387,990,306 published", got)
	}
	if len(s.Parts) != 5 {
		t.Fatalf("expected 5 parts, got %d", len(s.Parts))
	}

	for _, p := range s.Parts {
		if p.Bytes <= 0 {
			t.Errorf("%s has a length of %d", p.Name, p.Bytes)
		}
		// A GitHub release asset may not exceed two gibibytes. A part over the
		// limit could never have been uploaded, so a value above it means the
		// number is wrong rather than that the file is large.
		if p.Bytes > 2<<30 {
			t.Errorf("%s is %d bytes, which is more than a release asset may be",
				p.Name, p.Bytes)
		}
		assertSHA256(t, p.Name, p.SHA256)
	}
	assertSHA256(t, "the whole file", s.SHA256)
	assertSHA256(t, "the base block hash", s.BaseHash)

	if !strings.HasSuffix(s.BaseURL, "/") {
		t.Errorf("BaseURL %q does not end in a slash, so part names would be glued "+
			"onto the last path element", s.BaseURL)
	}
	if s.Network != "main" {
		t.Errorf("Network is %q; the checks that stop this being loaded into a test "+
			"network compare against what a node reports, which is \"main\"", s.Network)
	}
}

func assertSHA256(t *testing.T, what, value string) {
	t.Helper()
	if len(value) != 64 {
		t.Errorf("%s: %q is %d characters, not the 64 of a sha256 digest",
			what, value, len(value))
		return
	}
	if _, err := hex.DecodeString(value); err != nil {
		t.Errorf("%s: %q is not hexadecimal: %v", what, value, err)
	}
	if strings.ToLower(value) != value {
		t.Errorf("%s: %q is not lower case, and the comparison against a computed "+
			"digest is literal", what, value)
	}
}

// Every part name must be distinct and sort into concatenation order.
//
// The published instructions tell people to reassemble with `cat *.part`, which
// is shell glob order. If that disagreed with the order this package uses, the
// file somebody built by hand would differ from the one the daemon builds — and
// only one of them would load.
func TestPartNamesSortIntoConcatenationOrder(t *testing.T) {
	seen := map[string]bool{}
	previous := ""
	for i, p := range MainnetHeight935000.Parts {
		if seen[p.Name] {
			t.Errorf("part %d repeats the name %q", i, p.Name)
		}
		seen[p.Name] = true
		if previous != "" && p.Name <= previous {
			t.Errorf("part %d (%q) does not sort after %q, so a shell glob would "+
				"assemble the file in a different order than this package does",
				i, p.Name, previous)
		}
		previous = p.Name
	}
}

// PartAt is the whole of the resume logic. Everything else about restarting a
// download is derived from what it says about the length of the file on disk.
func TestPartAtLocatesAnOffsetInTheAssembledFile(t *testing.T) {
	s := Snapshot{Parts: []Part{
		{Name: "a", Bytes: 100},
		{Name: "b", Bytes: 50},
		{Name: "c", Bytes: 7},
	}}

	cases := []struct {
		name       string
		offset     int64
		wantIndex  int
		wantWithin int64
		wantOK     bool
	}{
		{"nothing downloaded", 0, 0, 0, true},
		{"partway through the first", 40, 0, 40, true},
		{"exactly on the first boundary", 100, 1, 0, true},
		{"partway through the second", 130, 1, 30, true},
		{"exactly on the second boundary", 150, 2, 0, true},
		{"one byte from the end", 156, 2, 6, true},
		{"complete", 157, 3, 0, true},
		{"longer than the snapshot", 158, 0, 0, false},
		{"negative", -1, 0, 0, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			index, within, ok := s.PartAt(c.offset)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if index != c.wantIndex || within != c.wantWithin {
				t.Errorf("PartAt(%d) = part %d at %d, want part %d at %d",
					c.offset, index, within, c.wantIndex, c.wantWithin)
			}
		})
	}
}

// BytesBefore is where a part gets truncated back to when it arrives corrupted.
// Off by one part and a retry would discard good data or keep bad.
func TestBytesBeforeIsTheStartOfEachPart(t *testing.T) {
	s := Snapshot{Parts: []Part{
		{Bytes: 100}, {Bytes: 50}, {Bytes: 7},
	}}
	for i, want := range []int64{0, 100, 150, 157} {
		if got := s.BytesBefore(i); got != want {
			t.Errorf("BytesBefore(%d) = %d, want %d", i, got, want)
		}
	}
}

func TestURLForJoinsTheBaseAndTheName(t *testing.T) {
	s := Snapshot{BaseURL: "https://example.invalid/releases/x/"}
	got := s.URLFor(Part{Name: "file.00.part"})
	if want := "https://example.invalid/releases/x/file.00.part"; got != want {
		t.Errorf("URLFor = %q, want %q", got, want)
	}
}
