package api

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/anchors"
)

// errUnexpected stands for a failure this handler has no specific wording for —
// the default branch still has to say something a person can act on.
var errUnexpected = errors.New("the disk caught fire")

// fakeAnchors stands in for the store, so the handler can be exercised without
// a filesystem.
type fakeAnchors struct {
	active   anchors.Active
	importFn func(raw, sig []byte) (anchors.List, error)
}

func (f *fakeAnchors) Load() anchors.Active { return f.active }

func (f *fakeAnchors) Import(raw, sig []byte) (anchors.List, error) {
	return f.importFn(raw, sig)
}

func anchorHarness(t *testing.T, a Anchors) *harness {
	t.Helper()
	h := newHarness(t, nil)
	h.srv.MountAnchors(a)
	return h
}

// The shipped state: an empty built-in list, shown as an empty list rather than
// as nothing.
func TestTheShippedAnchorListIsReportedAsEmptyNotMissing(t *testing.T) {
	h := anchorHarness(t, &fakeAnchors{
		active: anchors.Active{List: anchors.List{Source: anchors.SourceBuiltIn}},
	})

	got := decode[AnchorList](t, h.do(t, http.MethodGet, "/api/v1/anchors", ""))
	if got.Source != "built-in" {
		t.Errorf("source = %q", got.Source)
	}
	if got.Peers == nil {
		t.Error("peers is null; an empty list should say so with []")
	}
	if len(got.Peers) != 0 {
		t.Errorf("peers = %v, want empty", got.Peers)
	}
}

// A build with no signing key says so, so the page can decline to offer an
// import it could never accept.
func TestABuildWithNoKeyReportsThatItCannotImport(t *testing.T) {
	h := anchorHarness(t, &fakeAnchors{})

	got := decode[AnchorList](t, h.do(t, http.MethodGet, "/api/v1/anchors", ""))
	if got.CanImport {
		t.Error("a build with no key offered to import a list")
	}
	if got.Signer != "" {
		t.Errorf("signer = %q, want empty", got.Signer)
	}
}

// A fallback is reported, because a user who imported a list and is not running
// it has no other way to find out.
func TestAFallbackIsVisibleOnTheDashboard(t *testing.T) {
	h := anchorHarness(t, &fakeAnchors{
		active: anchors.Active{
			List:     anchors.List{Source: anchors.SourceBuiltIn},
			Fallback: "the signature does not match the list",
		},
	})

	got := decode[AnchorList](t, h.do(t, http.MethodGet, "/api/v1/anchors", ""))
	if got.Fallback == "" {
		t.Error("the page cannot tell the user their imported list is not in use")
	}
}

// **A refusal is a 200 with a reason.** The request was well formed; the list
// did not check out. That is an answer, and it is the answer most worth reading.
func TestARefusedImportIsAnAnswerNotAnError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		mustSay string
	}{
		{"a rollback", anchors.ErrNotNewer, "not newer"},
		{"a bad signature", anchors.ErrBadSignature, "does not match"},
		{"no key in this build", anchors.ErrNoSigningKey, "no key"},
		{"not a list at all", anchors.ErrNotAList, "not an anchor-peer list"},
		{"an unreadable signature", anchors.ErrUnreadableSignature, "not readable"},
		{"a newer format", anchors.ErrUnsupportedFormat, "newer version"},
		{"too many peers", anchors.ErrTooManyPeers, "more peers"},
		{"no version", anchors.ErrNoVersion, "not an anchor-peer list"},
		{"something else entirely", errUnexpected, "could not be used"},
	} {
		h := anchorHarness(t, &fakeAnchors{
			active:   anchors.Active{List: anchors.List{Version: 2, Source: anchors.SourceBuiltIn}},
			importFn: func(_, _ []byte) (anchors.List, error) { return anchors.List{}, tc.err },
		})

		resp := h.do(t, http.MethodPost, "/api/v1/anchors/import",
			`{"list":"whatever","signature":"aabb"}`)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", tc.name, resp.StatusCode)
			continue
		}
		got := decode[importResult](t, resp)
		if got.Accepted {
			t.Errorf("%s: a refused list was reported as accepted", tc.name)
		}
		if !strings.Contains(strings.ToLower(got.Reason), tc.mustSay) {
			t.Errorf("%s: reason %q does not say %q", tc.name, got.Reason, tc.mustSay)
		}
		// What is in use is returned either way, so the page never has to guess.
		if got.Active.Version != 2 {
			t.Errorf("%s: the active list was not reported after a refusal", tc.name)
		}
	}
}

// A good import reports what is now in use.
func TestAnAcceptedImportReportsTheNewList(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	previous := anchors.SigningKey
	anchors.SigningKey = hex.EncodeToString(pub)
	t.Cleanup(func() { anchors.SigningKey = previous })

	h := anchorHarness(t, &fakeAnchors{
		importFn: func(_, _ []byte) (anchors.List, error) {
			return anchors.List{Version: 9, Peers: []string{"a.onion:8333"}}, nil
		},
	})

	got := decode[importResult](t, h.do(t, http.MethodPost, "/api/v1/anchors/import",
		`{"list":"whatever","signature":"aabb"}`))
	if !got.Accepted {
		t.Fatalf("a good import was refused: %s", got.Reason)
	}
	if got.Active.Version != 9 || len(got.Active.Peers) != 1 {
		t.Errorf("active = %+v", got.Active)
	}
	if got.Active.Signer == "" {
		t.Error("no signer shown; a signature check is worth knowing whose")
	}
}

// Half a request is refused as a request, because it is one.
func TestAnImportNeedsBothHalves(t *testing.T) {
	h := anchorHarness(t, &fakeAnchors{})

	for _, body := range []string{`{"list":"x"}`, `{"signature":"y"}`, `{}`, `nonsense`} {
		resp := h.do(t, http.MethodPost, "/api/v1/anchors/import", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, resp.StatusCode)
		}
	}
}

// Without a store there are no routes at all.
func TestWithoutAnAnchorStoreThereIsNoAnchorApi(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.MountAnchors(nil)

	if resp := h.do(t, http.MethodGet, "/api/v1/anchors", ""); resp.StatusCode == http.StatusOK {
		t.Error("the anchor API answered without a store behind it")
	}
}
