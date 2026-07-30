package lnd

import (
	"encoding/hex"
	"sort"
	"testing"
)

// The identifier of a real read-only macaroon, read from a live LND node.
//
// Identifier only: the signature is what grants a macaroon its authority, and it
// stayed on that machine. What is here is a nonce, a storage id and a list of
// permissions — nothing that authorises anything.
//
// This fixture earns its place. The hand-built macaroons in the neighbouring
// test all passed while the decoder was wrong, because they used the version
// byte the format's history suggested (2) and real LND writes 3. Both sides of
// that test were mine, so they agreed with each other and not with reality.
const realReadOnlyIdentifier = "030a100aa0e8f5bd487d0f3b90ba0cc5966f891201301a0f0a07616464726573731204726561641a0c0a04696e666f1204726561641a100a08696e766f696365731204726561641a100a086d616361726f6f6e1204726561641a0f0a076d6573736167651204726561641a100a086f6666636861696e1204726561641a0f0a076f6e636861696e1204726561641a0d0a0570656572731204726561641a0e0a067369676e6572120472656164"

func TestAgainstARealMacaroon(t *testing.T) {
	t.Parallel()

	id, err := hex.DecodeString(realReadOnlyIdentifier)
	if err != nil {
		t.Fatal(err)
	}
	perms, ok := opsFrom(id)
	if !ok {
		t.Fatal("a real LND identifier could not be read")
	}
	var got []string
	for _, p := range perms {
		got = append(got, p.String())
	}
	sort.Strings(got)
	// Exactly what `lncli printmacaroon` reports for the same file.
	want := []string{
		"address:read", "info:read", "invoices:read", "macaroon:read",
		"message:read", "offchain:read", "onchain:read", "peers:read",
		"signer:read",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if (Credential{Permissions: perms, Readable: true}).Overprivileged() {
		t.Error("a read-only macaroon was reported as over-privileged")
	}
}
