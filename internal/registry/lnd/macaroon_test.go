package lnd

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// --- building macaroons to decode ---------------------------------------------
//
// Written from the two published formats rather than by reusing the decoder, so
// a mistake in one does not cancel out in the other. They are still both mine,
// which is the limit of what a unit test can establish here: the decoder is
// checked against a real LND macaroon in the integration work, where a live node
// exists.

func varint(v uint64) []byte {
	buf := make([]byte, binary.MaxVarintLen64)
	return buf[:binary.PutUvarint(buf, v)]
}

// protoField encodes one length-delimited protobuf field.
func protoField(num uint64, value []byte) []byte {
	out := varint(num<<3 | 2)
	out = append(out, varint(uint64(len(value)))...)
	return append(out, value...)
}

// bakeryOp encodes one Op{entity, actions...}.
func bakeryOp(entity string, actions ...string) []byte {
	out := protoField(1, []byte(entity))
	for _, a := range actions {
		out = append(out, protoField(2, []byte(a))...)
	}
	return out
}

// bakeryIdentifier encodes the identifier: a version byte, a nonce, a storage
// id, and the ops.
func bakeryIdentifier(ops ...[]byte) []byte {
	id := []byte{2}
	id = append(id, protoField(1, []byte("nonce-bytes"))...)
	id = append(id, protoField(2, []byte("storage-id"))...)
	for _, op := range ops {
		id = append(id, protoField(3, op)...)
	}
	return id
}

// macaroonV2 wraps an identifier in libmacaroons' v2 binary framing.
func macaroonV2(identifier []byte) []byte {
	out := []byte{2}
	field := func(t uint64, data []byte) {
		out = append(out, varint(t)...)
		out = append(out, varint(uint64(len(data)))...)
		out = append(out, data...)
	}
	field(fieldLocation, []byte("lnd"))
	field(fieldIdentifier, identifier)
	out = append(out, varint(fieldEOS)...)
	field(fieldSignature, make([]byte, 32))
	return out
}

// --- the tests ----------------------------------------------------------------

// The macaroon the setup instructions tell people to bake.
func TestAReadOnlyMacaroonReadsAsReadOnly(t *testing.T) {
	t.Parallel()

	raw := macaroonV2(bakeryIdentifier(
		bakeryOp("info", "read"),
		bakeryOp("offchain", "read"),
		bakeryOp("onchain", "read"),
		bakeryOp("peers", "read"),
	))

	perms, ok := DecodePermissions(raw)
	if !ok {
		t.Fatal("a well-formed macaroon could not be read")
	}
	if len(perms) != 4 {
		t.Fatalf("got %d permissions, want 4: %v", len(perms), perms)
	}

	cred := Credential{Permissions: perms, Readable: true}
	if cred.Overprivileged() {
		t.Errorf("a read-only macaroon was reported as over-privileged: %v", perms)
	}
	if !cred.Grants("offchain", "read") {
		t.Error("the macaroon should grant offchain:read")
	}
	if cred.Grants("offchain", "write") {
		t.Error("the macaroon should not grant offchain:write")
	}
}

// What both target platforms actually hand out. Reported loudly, but never a
// reason to refuse to start: refusing would mean no protection at all.
func TestAnAdminMacaroonIsReportedNotRefused(t *testing.T) {
	t.Parallel()

	raw := macaroonV2(bakeryIdentifier(
		bakeryOp("info", "read", "write"),
		bakeryOp("offchain", "read", "write"),
		bakeryOp("macaroon", "generate", "read", "write"),
	))

	perms, ok := DecodePermissions(raw)
	if !ok {
		t.Fatal("an admin macaroon could not be read")
	}
	cred := Credential{Permissions: perms, Readable: true}
	if !cred.Overprivileged() {
		t.Errorf("an admin macaroon was not reported as over-privileged: %v", perms)
	}
	// And the read permissions are still there, so the daemon can work.
	if !cred.Grants("offchain", "read") {
		t.Error("the read permissions were lost")
	}
}

// The distinction the whole file exists for: "we could not tell" must never
// present as "no write permissions found".
func TestAnUnreadableMacaroonIsNotReportedAsSafe(t *testing.T) {
	t.Parallel()

	cases := map[string][]byte{
		"empty":                {},
		"not the v2 format":    []byte("MDAxYmxvY2F0aW9uIGxuZAo"),
		"truncated framing":    {2, 1, 200},
		"identifier truncated": {2, 2, 200, 1, 2},
		"no ops in identifier": macaroonV2(bakeryIdentifier()),
		"identifier is not protobuf": macaroonV2(
			append([]byte{2}, []byte("a plain text identifier")...)),
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			perms, ok := DecodePermissions(raw)
			if ok {
				t.Errorf("reported as readable, giving %v", perms)
			}
			// The caller sees Readable=false, and must not mistake the empty list
			// for a clean bill of health.
			cred := Credential{Permissions: perms, Readable: ok}
			if cred.Readable {
				t.Error("an unreadable credential claims to be readable")
			}
			if cred.Overprivileged() {
				t.Error("an unreadable credential should make no claim either way")
			}
		})
	}
}

// A malformed op must fail the whole read rather than yield a partial list. Half
// a permission list read as complete is precisely the wrong answer here.
func TestAMalformedOpFailsTheWholeRead(t *testing.T) {
	t.Parallel()

	// A valid op followed by one whose action is missing.
	raw := macaroonV2(bakeryIdentifier(
		bakeryOp("info", "read"),
		protoField(1, []byte("offchain")), // entity, no actions
	))

	if perms, ok := DecodePermissions(raw); ok {
		t.Errorf("a malformed op produced %v and claimed success", perms)
	}
}

func TestLoadMacaroon(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "readonly.macaroon")
	raw := macaroonV2(bakeryIdentifier(bakeryOp("info", "read")))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	cred, err := LoadMacaroon(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cred.Readable || len(cred.Permissions) != 1 {
		t.Errorf("got %+v", cred)
	}
	if cred.Hex == "" {
		t.Error("the credential itself was not kept; it is what goes in the header")
	}

	if _, err := LoadMacaroon(filepath.Join(dir, "not-there")); err == nil {
		t.Error("a missing credential file was accepted")
	}

	empty := filepath.Join(dir, "empty.macaroon")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMacaroon(empty); err == nil {
		t.Error("an empty credential file was accepted")
	}
}

// Nothing that touches a credential may put it in an error: errors are logged,
// returned over the API, and carried in the support bundle.
func TestErrorsDoNotCarryTheCredential(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "bad.macaroon")
	secret := []byte("this-should-never-appear-in-an-error")
	if err := os.WriteFile(path, secret, 0o600); err != nil {
		t.Fatal(err)
	}

	cred, err := LoadMacaroon(path)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Readable {
		t.Fatal("nonsense was read as a macaroon")
	}
	// The bytes are held for the header, but nothing here formats them into a
	// message. Nothing to assert on the error because there is no error — which
	// is the point: an unreadable macaroon is a warning, not a failure.
	if cred.Hex == "" {
		t.Error("the credential was discarded")
	}
}
