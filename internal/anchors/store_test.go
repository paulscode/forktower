package anchors

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type storeHarness struct {
	t     *testing.T
	store *Store
	dir   string
	priv  ed25519.PrivateKey
}

func newStoreHarness(t *testing.T, builtInVersion int64) *storeHarness {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pin(t, hex.EncodeToString(pub))

	dir := t.TempDir()
	builtIn := filepath.Join(dir, "sq-anchors.txt")
	if err := os.WriteFile(builtIn, listBytes(builtInVersion), 0o600); err != nil {
		t.Fatal(err)
	}
	return &storeHarness{
		t: t, dir: dir, priv: priv,
		store: NewStore(dir, builtIn, nil),
	}
}

func (h *storeHarness) sign(raw []byte) []byte {
	h.t.Helper()
	return []byte(hex.EncodeToString(ed25519.Sign(h.priv, raw)))
}

// With nothing imported, the shipped list is what is used and nothing is said
// about it — that is the ordinary state, not a problem.
func TestTheShippedListIsUsedWhenNothingWasImported(t *testing.T) {
	h := newStoreHarness(t, 0)

	got := h.store.Load()
	if got.Source != SourceBuiltIn {
		t.Errorf("source = %q, want %q", got.Source, SourceBuiltIn)
	}
	if got.Fallback != "" {
		t.Errorf("an ordinary start reported a fallback: %q", got.Fallback)
	}
	if !got.Empty() {
		t.Error("the shipped empty list did not read as empty")
	}
}

// An imported list survives a restart, because that is the point of writing it.
func TestAnImportedListIsUsedAfterwards(t *testing.T) {
	h := newStoreHarness(t, 0)
	raw := listBytes(4, "aaa.onion:8333", "bbb.onion:8333")

	if _, err := h.store.Import(raw, h.sign(raw)); err != nil {
		t.Fatalf("a good list was refused: %v", err)
	}

	// A second Store over the same directory, which is what a restart is.
	fresh := NewStore(h.dir, filepath.Join(h.dir, "sq-anchors.txt"), nil)
	got := fresh.Load()
	if got.Source != SourceImported || got.Version != 4 {
		t.Errorf("after a restart: source %q version %d, want imported version 4",
			got.Source, got.Version)
	}
	if len(got.Peers) != 2 {
		t.Errorf("got %d peers, want 2", len(got.Peers))
	}
}

// **A list that was verified once is not trusted for ever.**
//
// The file sits in a data directory that a backup restore, a misplaced volume
// mount or anybody with a shell could have rewritten since. So it is checked
// again every time it is read — and when it fails, the built-in list is used and
// the user is told, rather than the daemon quietly running peers somebody else
// chose.
func TestAListEditedAfterImportIsCaughtOnTheNextLoad(t *testing.T) {
	h := newStoreHarness(t, 0)
	raw := listBytes(4, "good.onion:8333")
	if _, err := h.store.Import(raw, h.sign(raw)); err != nil {
		t.Fatal(err)
	}

	// Somebody with a shell swaps a peer for their own.
	path := filepath.Join(h.dir, activeDirName, activeListName)
	edited := strings.Replace(string(raw), "good.onion", "evil.onion", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	got := h.store.Load()
	if got.Source != SourceBuiltIn {
		t.Errorf("an edited list was still in use: source %q, peers %v",
			got.Source, got.Peers)
	}
	for _, p := range got.Peers {
		if strings.Contains(p, "evil") {
			t.Fatalf("the attacker's peer survived: %v", got.Peers)
		}
	}
	if got.Fallback == "" {
		t.Error("the fallback was silent; a user would think their list was in use")
	}
}

// A properly signed rollback is refused at the door, not on the next load.
func TestImportRefusesAnOlderListThanTheOneShipped(t *testing.T) {
	h := newStoreHarness(t, 9)
	raw := listBytes(4, "old.onion:8333")

	if _, err := h.store.Import(raw, h.sign(raw)); !errors.Is(err, ErrNotNewer) {
		t.Errorf("error = %v, want ErrNotNewer", err)
	}
	if got := h.store.Load(); got.Source != SourceBuiltIn {
		t.Errorf("a refused import changed what is in use: %q", got.Source)
	}
}

// A refused import leaves the previous list exactly as it was.
func TestARefusedImportDoesNotDisturbWhatIsInUse(t *testing.T) {
	h := newStoreHarness(t, 0)
	good := listBytes(4, "good.onion:8333")
	if _, err := h.store.Import(good, h.sign(good)); err != nil {
		t.Fatal(err)
	}

	// Signed by somebody else entirely.
	_, otherPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	bad := listBytes(5, "evil.onion:8333")
	badSig := []byte(hex.EncodeToString(ed25519.Sign(otherPriv, bad)))

	if _, err := h.store.Import(bad, badSig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("error = %v, want ErrBadSignature", err)
	}

	got := h.store.Load()
	if got.Version != 4 || len(got.Peers) != 1 || got.Peers[0] != "good.onion:8333" {
		t.Errorf("the refused import disturbed the list in use: %+v", got.List)
	}
}

// A build whose shipped list is missing or unreadable still runs.
//
// It is a build with no anchors, which is the shipped state anyway — the node
// finds peers the ordinary way, exactly as it would have done regardless.
func TestAMissingShippedListIsNotAFailure(t *testing.T) {
	pin(t, "")
	s := NewStore(t.TempDir(), "/nonexistent/sq-anchors.txt", nil)

	got := s.Load()
	if !got.Empty() || got.Source != SourceBuiltIn {
		t.Errorf("got %+v, want an empty built-in list", got.List)
	}
}

// A shipped list that is not a list is reported, not obeyed.
//
// A build whose own anchor file was mangled — by a bad edit, a broken packaging
// step — must not silently behave as though it shipped peers it did not.
func TestAnUnreadableShippedListFallsBackToNoAnchors(t *testing.T) {
	pin(t, "")
	dir := t.TempDir()
	builtIn := filepath.Join(dir, "sq-anchors.txt")
	if err := os.WriteFile(builtIn, []byte("aaa.onion:8333\nbbb.onion:8333\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := NewStore(dir, builtIn, nil).Load()
	if !got.Empty() {
		t.Errorf("an unreadable shipped list produced peers: %v", got.Peers)
	}
}

// A list with no signature beside it is not a list that can be used.
//
// The two files are written together and are useless apart; finding one without
// the other means something interrupted a write or removed a file, and either
// way there is nothing to verify against.
func TestAnImportedListWithNoSignatureIsNotUsed(t *testing.T) {
	h := newStoreHarness(t, 0)
	dir := filepath.Join(h.dir, activeDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := listBytes(4, "aaa.onion:8333")
	if err := os.WriteFile(filepath.Join(dir, activeListName), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	got := h.store.Load()
	if got.Source != SourceBuiltIn {
		t.Errorf("a list with no signature was used: %+v", got.List)
	}
	if got.Fallback == "" {
		t.Error("the fallback was silent")
	}
}

// An import that cannot be written reports that, rather than claiming success.
func TestAnImportThatCannotBeWrittenIsReported(t *testing.T) {
	h := newStoreHarness(t, 0)

	// A file where the anchors directory needs to be, so creating it fails.
	if err := os.WriteFile(filepath.Join(h.dir, activeDirName), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}

	raw := listBytes(4, "aaa.onion:8333")
	if _, err := h.store.Import(raw, h.sign(raw)); err == nil {
		t.Fatal("an import that could not be written reported success")
	}
}
