package anchors

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Errors from verification, kept apart so a caller can say which thing failed.
var (
	// ErrNoSigningKey means this build has no pinned key, so nothing can be
	// imported. Not a failure of the list.
	ErrNoSigningKey = errors.New("anchors: this build has no anchor-list signing key")
	// ErrBadSignature means the signature does not match the bytes.
	ErrBadSignature = errors.New("anchors: the signature does not match the list")
	// ErrUnreadableSignature means the signature file is not a signature.
	ErrUnreadableSignature = errors.New("anchors: unreadable signature")
	// ErrNotNewer means the list verified but does not supersede the active one.
	ErrNotNewer = errors.New("anchors: the list is not newer than the one in use")
)

// SigningKey is the Ed25519 public key this build trusts, as lowercase hex.
//
// **Set at build time and empty by default.** An empty key means no list can be
// imported at all, which is the right behaviour for a build that was not given
// one: the alternative — accepting anything because there is nothing to check
// against — would turn a missing key into an open door.
//
//nolint:gochecknoglobals // set with -ldflags at build time; there is nowhere else for it to live
var SigningKey = ""

// Fingerprint is the short form of the pinned key, for showing a user which key
// their software trusts.
//
// The first eight bytes. Long enough that nobody is producing a collision to
// fool a human reading a dashboard, short enough that a human will actually
// compare it.
func Fingerprint() string {
	key, err := publicKey()
	if err != nil {
		return ""
	}
	return hex.EncodeToString(key[:8])
}

// HaveSigningKey reports whether this build can verify an imported list.
func HaveSigningKey() bool {
	_, err := publicKey()
	return err == nil
}

func publicKey() (ed25519.PublicKey, error) {
	trimmed := strings.TrimSpace(SigningKey)
	if trimmed == "" {
		return nil, ErrNoSigningKey
	}
	raw, err := hex.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%w: the pinned key is not hex", ErrNoSigningKey)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: the pinned key is %d bytes, want %d",
			ErrNoSigningKey, len(raw), ed25519.PublicKeySize)
	}
	return raw, nil
}

// Accept decides whether an imported list may replace the one in use.
//
// Four things have to hold, and the order is deliberate — each answers a
// different question and a caller wants to know which one failed:
//
//  1. this build has a key to check against;
//  2. the signature is over exactly these bytes;
//  3. the bytes are a list this build can read;
//  4. the list is newer than the one in use.
//
// The fourth is the one that is easy to leave out and the one that costs money
// without it. A signed list is not a *current* list: an attacker who can hand
// the daemon a file can hand it a genuine, properly signed list from a year ago,
// whose peers have all since gone dark. The node would then start with a set of
// addresses that answer nothing, and a second chain nobody can reach looks
// exactly like a second chain where nothing is happening.
//
// Returns the accepted list, or an error naming which check failed. On any
// error the caller keeps what it had — there is no partial acceptance, because
// half a peer list is not a safer peer list.
func Accept(raw, signature []byte, current List) (List, error) {
	key, err := publicKey()
	if err != nil {
		return List{}, err
	}

	sig, err := decodeSignature(signature)
	if err != nil {
		return List{}, err
	}

	// Verified before parsing, on the raw bytes. Parsing first would mean this
	// build's parser had already run on unauthenticated input, and would leave
	// the signature covering an interpretation rather than a file.
	if !ed25519.Verify(key, raw, sig) {
		return List{}, ErrBadSignature
	}

	candidate, err := Parse(raw)
	if err != nil {
		return List{}, err
	}

	if !candidate.Supersedes(current) {
		return List{}, fmt.Errorf("%w: it is version %d and version %d is in use",
			ErrNotNewer, candidate.Version, current.Version)
	}

	candidate.Source = SourceImported
	return candidate, nil
}

// decodeSignature reads a detached signature, as hex with any surrounding
// whitespace ignored — a file a person may have copied by hand.
func decodeSignature(signature []byte) ([]byte, error) {
	text := strings.TrimSpace(string(signature))
	raw, err := hex.DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("%w: not hex", ErrUnreadableSignature)
	}
	if len(raw) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: %d bytes, want %d",
			ErrUnreadableSignature, len(raw), ed25519.SignatureSize)
	}
	return raw, nil
}
