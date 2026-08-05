package anchors

import (
	"errors"
	"strings"
	"testing"
)

// The ordinary shape, including the things a person writes by hand.
func TestParseReadsAListWithCommentsAndBlanks(t *testing.T) {
	got, err := Parse([]byte(`
# Anchor peers.
#
` + FormatTag + `: 1
version: 12

peer: aaa.onion:8333
peer: bbb.onion:8333
`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 12 {
		t.Errorf("version = %d, want 12", got.Version)
	}
	if len(got.Peers) != 2 || got.Peers[0] != "aaa.onion:8333" {
		t.Errorf("peers = %v", got.Peers)
	}
}

// An empty list is valid, and is the shipped state.
//
// Naming nodes that have since gone dark is worse than naming none, because it
// looks like a measure that is working. Parsing has to accept that rather than
// treat it as a broken file.
func TestAnEmptyListIsValid(t *testing.T) {
	got, err := Parse([]byte(FormatTag + ": 1\nversion: 0\n"))
	if err != nil {
		t.Fatalf("the shipped empty list does not parse: %v", err)
	}
	if !got.Empty() {
		t.Error("an empty list does not report itself empty")
	}
}

// Strict on purpose: these bytes may have been handed over by someone hostile,
// and being generous here produces a peer list that does not say what its author
// thought it said.
func TestParseRefusesWhatItCannotVouchFor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		wantErr error
	}{
		{
			name:    "not an anchor list at all",
			input:   "aaa.onion:8333\nbbb.onion:8333\n",
			wantErr: ErrNotAList,
		},
		{
			name:    "no format directive",
			input:   "version: 3\npeer: aaa.onion:8333\n",
			wantErr: ErrNotAList,
		},
		{
			name:    "no version, so rollback could not be detected",
			input:   FormatTag + ": 1\npeer: aaa.onion:8333\n",
			wantErr: ErrNoVersion,
		},
		{
			name:    "a version that is not a number",
			input:   FormatTag + ": 1\nversion: soon\n",
			wantErr: ErrNoVersion,
		},
		{
			name:    "a negative version",
			input:   FormatTag + ": 1\nversion: -1\n",
			wantErr: ErrNoVersion,
		},
		{
			name:    "a format this build does not read",
			input:   FormatTag + ": 2\nversion: 3\n",
			wantErr: ErrUnsupportedFormat,
		},
		{
			name:    "a key this build does not know",
			input:   FormatTag + ": 1\nversion: 3\ntrust: everything\n",
			wantErr: ErrNotAList,
		},
		{
			name:    "an empty peer",
			input:   FormatTag + ": 1\nversion: 3\npeer:\n",
			wantErr: ErrNotAList,
		},
		{
			name: "more peers than a node should be asked to hold",
			input: FormatTag + ": 1\nversion: 3\n" +
				strings.Repeat("peer: a.onion:8333\n", MaxPeers+1),
			wantErr: ErrTooManyPeers,
		},
	} {
		if _, err := Parse([]byte(tc.input)); !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: error = %v, want %v", tc.name, err, tc.wantErr)
		}
	}
}

// A newer format is refused rather than read best-effort.
//
// It may mean something by a field this build would silently ignore, and
// silently ignoring part of a peer list is how a node ends up peering with less
// than its operator was told.
func TestANewerFormatIsRefusedRatherThanPartlyRead(t *testing.T) {
	_, err := Parse([]byte(FormatTag + ": 99\nversion: 3\npeer: aaa.onion:8333\n"))
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("error = %v, want ErrUnsupportedFormat", err)
	}
}

// Strictly greater. Equal is refused, because two different lists at one version
// is either a mistake by whoever signs them or an attempt to swap one for the
// other without moving the counter.
func TestSupersedesIsStrict(t *testing.T) {
	for _, tc := range []struct {
		candidate, current int64
		want               bool
	}{
		{7, 3, true},
		{3, 7, false},
		{7, 7, false},
		{0, 0, false},
	} {
		got := List{Version: tc.candidate}.Supersedes(List{Version: tc.current})
		if got != tc.want {
			t.Errorf("version %d over %d = %v, want %v",
				tc.candidate, tc.current, got, tc.want)
		}
	}
}
