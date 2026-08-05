package anchors

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// signedList builds a list file and a signature over it, and pins the matching
// public key into the package for the duration of the test.
func signedList(t *testing.T, version int64, peers ...string) (raw, sig []byte) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pin(t, hex.EncodeToString(pub))

	raw = listBytes(version, peers...)
	return raw, []byte(hex.EncodeToString(ed25519.Sign(priv, raw)))
}

func listBytes(version int64, peers ...string) []byte {
	var b strings.Builder
	b.WriteString("# a list\n")
	b.WriteString(FormatTag + ": 1\n")
	b.WriteString("version: ")
	b.WriteString(itoa(version))
	b.WriteString("\n")
	for _, p := range peers {
		b.WriteString("peer: " + p + "\n")
	}
	return []byte(b.String())
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// pin sets the build-time key for one test and puts it back afterwards.
func pin(t *testing.T, key string) {
	t.Helper()
	previous := SigningKey
	SigningKey = key
	t.Cleanup(func() { SigningKey = previous })
}

// The ordinary case: a properly signed, newer list replaces the one in use.
func TestANewerSignedListIsAccepted(t *testing.T) {
	raw, sig := signedList(t, 7, "aaa.onion:8333", "bbb.onion:8333")

	got, err := Accept(raw, sig, List{Version: 3, Source: SourceBuiltIn})
	if err != nil {
		t.Fatalf("a good list was refused: %v", err)
	}
	if got.Version != 7 {
		t.Errorf("version = %d, want 7", got.Version)
	}
	if len(got.Peers) != 2 {
		t.Errorf("got %d peers, want 2", len(got.Peers))
	}
	if got.Source != SourceImported {
		t.Errorf("source = %q, want %q", got.Source, SourceImported)
	}
}

// **A tampered list is refused, and the caller keeps what it had.**
//
// The whole point of the signature. Somebody who can replace this file can point
// the second node at peers of their choosing, and a second chain reached only
// through an attacker's peers can be made to look like a chain where nothing is
// happening — which is the one lie that costs a user money.
func TestATamperedListIsRefused(t *testing.T) {
	raw, sig := signedList(t, 7, "aaa.onion:8333")

	for _, tc := range []struct {
		name    string
		mutate  func([]byte) []byte
		wantErr error
	}{
		{
			name: "a peer swapped for the attacker's",
			mutate: func(b []byte) []byte {
				return []byte(strings.Replace(string(b), "aaa.onion", "evil.onion", 1))
			},
			wantErr: ErrBadSignature,
		},
		{
			name: "a peer appended",
			mutate: func(b []byte) []byte {
				return append(b, []byte("peer: evil.onion:8333\n")...)
			},
			wantErr: ErrBadSignature,
		},
		{
			name: "the version raised to force acceptance",
			mutate: func(b []byte) []byte {
				return []byte(strings.Replace(string(b), "version: 7", "version: 99", 1))
			},
			wantErr: ErrBadSignature,
		},
		{
			name:    "a single byte flipped",
			mutate:  func(b []byte) []byte { c := append([]byte{}, b...); c[0] ^= 0x01; return c },
			wantErr: ErrBadSignature,
		},
	} {
		current := List{Version: 3, Source: SourceBuiltIn}
		got, err := Accept(tc.mutate(raw), sig, current)
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: error = %v, want %v", tc.name, err, tc.wantErr)
		}
		if !got.Empty() || got.Version != 0 {
			t.Errorf("%s: a refused list was returned anyway: %+v", tc.name, got)
		}
	}
}

// **A rollback is refused even though it is properly signed.**
//
// This is the check that is easy to leave out, because every signature involved
// is genuine. An attacker who can hand the daemon a file can hand it a real,
// correctly signed list from a year ago whose peers have all since gone dark —
// and a second chain nobody can reach looks exactly like a second chain where
// nothing is happening.
func TestAProperlySignedRollbackIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int64
		current int64
	}{
		{"older than the one in use", 3, 7},
		{"the same version, different peers", 7, 7},
	} {
		raw, sig := signedList(t, tc.version, "old.onion:8333")

		_, err := Accept(raw, sig, List{Version: tc.current, Source: SourceBuiltIn})
		if !errors.Is(err, ErrNotNewer) {
			t.Errorf("%s: error = %v, want ErrNotNewer", tc.name, err)
		}
	}
}

// A signature made by the wrong key is not a signature.
func TestAListSignedByAnotherKeyIsRefused(t *testing.T) {
	raw, _ := signedList(t, 7, "aaa.onion:8333")

	// Somebody else's key, signing the same bytes perfectly well.
	_, otherPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	otherSig := []byte(hex.EncodeToString(ed25519.Sign(otherPriv, raw)))

	if _, err := Accept(raw, otherSig, List{}); !errors.Is(err, ErrBadSignature) {
		t.Errorf("error = %v, want ErrBadSignature", err)
	}
}

// A build with no key imports nothing.
//
// The alternative — accepting anything, because there is nothing to check
// against — would turn a missing key into an open door, and it would do it on
// exactly the builds nobody was paying attention to.
func TestABuildWithNoKeyImportsNothing(t *testing.T) {
	raw, sig := signedList(t, 7, "aaa.onion:8333")
	pin(t, "")

	if _, err := Accept(raw, sig, List{}); !errors.Is(err, ErrNoSigningKey) {
		t.Errorf("error = %v, want ErrNoSigningKey", err)
	}
	if HaveSigningKey() {
		t.Error("a build with no key says it has one")
	}
	if Fingerprint() != "" {
		t.Error("a build with no key showed a fingerprint")
	}
}

// A key that is not a key is the same as no key, rather than a panic.
func TestAMalformedPinnedKeyIsTreatedAsNone(t *testing.T) {
	raw, sig := signedList(t, 7, "aaa.onion:8333")

	for _, key := range []string{"not-hex", "aabb", strings.Repeat("aa", 31)} {
		pin(t, key)
		if _, err := Accept(raw, sig, List{}); !errors.Is(err, ErrNoSigningKey) {
			t.Errorf("key %q: error = %v, want ErrNoSigningKey", key, err)
		}
	}
}

// An unreadable signature is refused before anything else is trusted.
func TestAnUnreadableSignatureIsRefused(t *testing.T) {
	raw, _ := signedList(t, 7, "aaa.onion:8333")

	for _, sig := range []string{"", "zzzz", "aabbcc"} {
		if _, err := Accept(raw, []byte(sig), List{}); !errors.Is(err, ErrUnreadableSignature) {
			t.Errorf("signature %q: error = %v, want ErrUnreadableSignature", sig, err)
		}
	}
}

// The fingerprint is stable and short enough that somebody will compare it.
func TestTheFingerprintIdentifiesTheKey(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pin(t, hex.EncodeToString(pub))

	got := Fingerprint()
	if len(got) != 16 {
		t.Errorf("fingerprint %q is %d characters; 16 is short enough to read and "+
			"long enough not to collide", got, len(got))
	}
	if got != Fingerprint() {
		t.Error("the fingerprint changed between calls")
	}
	if !strings.HasPrefix(hex.EncodeToString(pub), got) {
		t.Error("the fingerprint is not a prefix of the key it names")
	}
}
