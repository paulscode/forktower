package tower

import (
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/store"
)

func TestVersionsAreReadFromWhateverLNDReports(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		raw               string
		known             bool
		maj, min, pat     int
		atLeast018        bool
		atLeast012        bool
		describeContains  string
		describeIsUnknown bool
	}{
		{raw: "0.18.5-beta commit=v0.18.5-beta", known: true, maj: 0, min: 18, pat: 5, atLeast018: true, atLeast012: true},
		{raw: "0.17.5-beta", known: true, maj: 0, min: 17, pat: 5, atLeast018: false, atLeast012: true},
		{raw: "0.11.1-beta", known: true, maj: 0, min: 11, pat: 1, atLeast018: false, atLeast012: false},
		{raw: "v0.21.1-beta", known: true, maj: 0, min: 21, pat: 1, atLeast018: true, atLeast012: true},
		{raw: "0.20.2", known: true, maj: 0, min: 20, pat: 2, atLeast018: true, atLeast012: true},
		{raw: "1.0.0", known: true, maj: 1, min: 0, pat: 0, atLeast018: true, atLeast012: true},
		{raw: "", known: false, describeIsUnknown: true},
		{raw: "unreleased", known: false, describeIsUnknown: true},
		{raw: "0", known: false, describeIsUnknown: true},
	} {
		v := ParseVersion(tc.raw)
		if v.Known != tc.known {
			t.Errorf("%q: known = %v, want %v", tc.raw, v.Known, tc.known)
			continue
		}
		if !tc.known {
			if !strings.Contains(v.String(), "unknown") {
				t.Errorf("%q: describes itself as %q", tc.raw, v.String())
			}
			continue
		}
		if v.Major != tc.maj || v.Minor != tc.min || v.Patch != tc.pat {
			t.Errorf("%q parsed as %d.%d.%d, want %d.%d.%d",
				tc.raw, v.Major, v.Minor, v.Patch, tc.maj, tc.min, tc.pat)
		}
		if got := v.atLeast(0, 18); got != tc.atLeast018 {
			t.Errorf("%q: at least 0.18 = %v, want %v", tc.raw, got, tc.atLeast018)
		}
		if got := v.atLeast(0, 12); got != tc.atLeast012 {
			t.Errorf("%q: at least 0.12 = %v, want %v", tc.raw, got, tc.atLeast012)
		}
	}
}

// A version we could not read must never satisfy a requirement. Treating it as
// "probably fine" is how a channel gets reported protected by a session nothing
// could create.
func TestAnUnreadableVersionSupportsNothing(t *testing.T) {
	t.Parallel()
	unknown := ParseVersion("who knows")

	for _, p := range []PolicyType{PolicyLegacy, PolicyAnchor, PolicyTaproot, PolicyUnknown} {
		if Supports(unknown, p) {
			t.Errorf("an unreadable version was said to support %q", p)
		}
	}
}

// The table measured from LND's own tagged source.
func TestTheVersionMatrixMatchesWhatLNDActuallyDoes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		version                        string
		legacy, anchor, taproot        bool
		describeWhyIfThisEverGoesWrong string
	}{
		{"0.11.1-beta", true, false, false, "anchor arrived in 0.12.0"},
		{"0.12.0-beta", true, true, false, "taproot arrived in 0.18.0"},
		{"0.17.5-beta", true, true, false, "the last release before taproot sessions"},
		{"0.18.0-beta", true, true, true, "the first release covering all three"},
		{"0.18.5-beta", true, true, true, "what we pin the companion tower to"},
		{"0.21.1-beta", true, true, true, "taproot-final is a separate type, not modelled"},
	} {
		v := ParseVersion(tc.version)
		if got := Supports(v, PolicyLegacy); got != tc.legacy {
			t.Errorf("%s legacy = %v, want %v (%s)", tc.version, got, tc.legacy, tc.describeWhyIfThisEverGoesWrong)
		}
		if got := Supports(v, PolicyAnchor); got != tc.anchor {
			t.Errorf("%s anchor = %v, want %v (%s)", tc.version, got, tc.anchor, tc.describeWhyIfThisEverGoesWrong)
		}
		if got := Supports(v, PolicyTaproot); got != tc.taproot {
			t.Errorf("%s taproot = %v, want %v (%s)", tc.version, got, tc.taproot, tc.describeWhyIfThisEverGoesWrong)
		}
	}
}

func TestAChannelTypeDecidesItsSessionType(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		chanType store.ChanType
		want     PolicyType
	}{
		{store.ChanLegacy, PolicyLegacy},
		{store.ChanStaticRemote, PolicyLegacy},
		{store.ChanAnchors, PolicyAnchor},
		{store.ChanTaproot, PolicyTaproot},
		{store.ChanTypeUnknown, PolicyUnknown},
		{store.ChanType("something new"), PolicyUnknown},
	} {
		if got := PolicyForChannel(tc.chanType); got != tc.want {
			t.Errorf("%q needs %q, want %q", tc.chanType, got, tc.want)
		}
	}
}

// A held session is evidence and beats any inference from version numbers.
func TestAHeldSessionIsWhatMakesAChannelCovered(t *testing.T) {
	t.Parallel()

	v := ParseVersion("0.18.5-beta")
	got := Assess(Inputs{
		ChanType:        store.ChanAnchors,
		TowerVersion:    v,
		ClientVersion:   v,
		SessionPolicies: map[PolicyType]bool{PolicyAnchor: true},
	})
	if !got.Coverable {
		t.Fatalf("a channel with a matching session was reported uncovered: %s", got.Reason)
	}
	if got.Policy != PolicyAnchor {
		t.Errorf("policy = %q, want %q", got.Policy, PolicyAnchor)
	}
	if !strings.Contains(got.Reason, "anchor session") {
		t.Errorf("the reason does not say what makes it covered: %q", got.Reason)
	}
}

// The silent failure R2 found: the wrong session type covers nothing, while
// every other channel backs up normally and the tower looks healthy.
func TestAChannelWhoseSessionTypeIsMissingIsNotCovered(t *testing.T) {
	t.Parallel()

	v := ParseVersion("0.18.5-beta")
	got := Assess(Inputs{
		ChanType:      store.ChanTaproot,
		TowerVersion:  v,
		ClientVersion: v,
		// Anchor sessions exist and are irrelevant to a taproot channel.
		SessionPolicies:      map[PolicyType]bool{PolicyAnchor: true, PolicyLegacy: true},
		RegisteredForSeconds: GracePeriodSeconds * 2,
	})
	if got.Coverable {
		t.Error("a taproot channel was reported covered by anchor and legacy sessions")
	}
	if got.StillSettling {
		t.Error("a long-registered tower was reported as still settling")
	}
	if !strings.Contains(got.Reason, "not negotiated") {
		t.Errorf("the reason does not explain the gap: %q", got.Reason)
	}
}

// Which end cannot do it decides what the user has to go and change.
func TestTheReasonNamesTheEndThatCannotDoIt(t *testing.T) {
	t.Parallel()

	newEnough := ParseVersion("0.18.5-beta")
	tooOld := ParseVersion("0.17.5-beta")

	theirs := Assess(Inputs{
		ChanType: store.ChanTaproot, TowerVersion: newEnough, ClientVersion: tooOld,
		RegisteredForSeconds: GracePeriodSeconds * 2,
	})
	if theirs.Coverable {
		t.Error("a taproot channel on a node that cannot make taproot sessions was covered")
	}
	if !strings.Contains(theirs.Reason, "Lightning node is running") {
		t.Errorf("the reason does not blame the node: %q", theirs.Reason)
	}
	if !strings.Contains(theirs.Reason, "0.17.5") {
		t.Errorf("the reason does not quote the version we were given: %q", theirs.Reason)
	}

	ours := Assess(Inputs{
		ChanType: store.ChanTaproot, TowerVersion: tooOld, ClientVersion: newEnough,
		RegisteredForSeconds: GracePeriodSeconds * 2,
	})
	if !strings.Contains(ours.Reason, "the tower is running") {
		t.Errorf("the reason does not blame the tower: %q", ours.Reason)
	}
}

// A tower registered moments ago with no sessions has not failed: LND's
// negotiator backs off exponentially and can take minutes.
func TestAFreshRegistrationIsGivenTimeBeforeItIsCalledAFailure(t *testing.T) {
	t.Parallel()

	v := ParseVersion("0.18.5-beta")
	in := Inputs{ChanType: store.ChanAnchors, TowerVersion: v, ClientVersion: v}

	in.RegisteredForSeconds = 30
	fresh := Assess(in)
	if fresh.Coverable {
		t.Error("a channel with no session was reported covered during the grace period")
	}
	if !fresh.StillSettling {
		t.Error("a tower registered 30 seconds ago was not treated as still settling")
	}
	if !strings.Contains(fresh.Reason, "not negotiated immediately") {
		t.Errorf("the reason does not explain the wait: %q", fresh.Reason)
	}

	in.RegisteredForSeconds = GracePeriodSeconds + 1
	settled := Assess(in)
	if settled.StillSettling {
		t.Error("a tower past the grace period was still being excused")
	}
}

// The grace period has to outlast LND's own backoff, which was measured at
// 2m30s in the worst observed case.
func TestTheGracePeriodOutlastsTheObservedBackoff(t *testing.T) {
	t.Parallel()
	const observedWorstCaseSeconds = 150
	if GracePeriodSeconds <= observedWorstCaseSeconds {
		t.Errorf("the grace period is %ds, which does not outlast the %ds backoff "+
			"actually measured — a healthy tower would be reported broken",
			GracePeriodSeconds, observedWorstCaseSeconds)
	}
}

// A channel whose type we could not read must not be reported protected.
func TestAnUnknownChannelTypeIsNotQuietlyAssumedCovered(t *testing.T) {
	t.Parallel()

	v := ParseVersion("0.18.5-beta")
	got := Assess(Inputs{
		ChanType: store.ChanTypeUnknown, TowerVersion: v, ClientVersion: v,
		// Every session type in the world, and it still cannot be said.
		SessionPolicies: map[PolicyType]bool{
			PolicyLegacy: true, PolicyAnchor: true, PolicyTaproot: true,
		},
		RegisteredForSeconds: GracePeriodSeconds * 2,
	})
	if got.Coverable {
		t.Error("a channel of unknown type was reported covered")
	}
	if !strings.Contains(got.Reason, "not known") {
		t.Errorf("the reason does not say what is missing: %q", got.Reason)
	}
}

// Every verdict carries a reason, whichever way it went.
func TestEveryVerdictExplainsItself(t *testing.T) {
	t.Parallel()

	versions := []Version{
		ParseVersion("0.11.1-beta"), ParseVersion("0.12.0-beta"),
		ParseVersion("0.18.5-beta"), ParseVersion("nonsense"),
	}
	chanTypes := []store.ChanType{
		store.ChanLegacy, store.ChanStaticRemote, store.ChanAnchors,
		store.ChanTaproot, store.ChanTypeUnknown,
	}
	sessionSets := []map[PolicyType]bool{
		nil,
		{PolicyLegacy: true},
		{PolicyAnchor: true},
		{PolicyLegacy: true, PolicyAnchor: true, PolicyTaproot: true},
	}

	for _, tower := range versions {
		for _, client := range versions {
			for _, ct := range chanTypes {
				for _, sessions := range sessionSets {
					for _, age := range []int64{0, GracePeriodSeconds * 2} {
						got := Assess(Inputs{
							ChanType: ct, TowerVersion: tower, ClientVersion: client,
							SessionPolicies: sessions, RegisteredForSeconds: age,
						})
						if strings.TrimSpace(got.Reason) == "" {
							t.Fatalf("no reason for %v/%v/%v/%v/%d",
								ct, tower.Raw, client.Raw, sessions, age)
						}
						if got.Coverable && got.StillSettling {
							t.Fatalf("covered and still settling at once: %+v", got)
						}
					}
				}
			}
		}
	}
}

// A tower that cannot accept a session must not be reported as a node that has
// not asked for one.
//
// **Measured on real hardware, and the highest-volume complaint this project
// has had.** A user's lnd dialled the companion tower forty-two times and timed
// out on every read, reporting zero sessions on all three policy types — while
// the tower sat at "Waiting for chain backend to finish sync", its own Bitcoin
// node 2.4% of the way through an initial sync. The verdict blamed the node:
// "both ends support anchor sessions but the node has not negotiated one with
// this tower". Nothing about that was the node's doing, and there was nothing
// the user could act on.
//
// This is close to universal on a fresh install, where the second node is
// always still syncing, and it lasts for days.
func TestATowerThatCannotServeIsNotReportedAsTheNodesFault(t *testing.T) {
	t.Parallel()
	const why = "the tower's own Bitcoin node is still catching up (height 289556), " +
		"and the tower does not accept sessions or watch for breaches until it has finished"

	got := Assess(Inputs{
		ChanType:      store.ChanAnchors,
		TowerVersion:  ParseVersion("0.18.5-beta"),
		ClientVersion: ParseVersion("0.18.5-beta"),
		// Long past the grace period, which is the case that used to blame the node.
		RegisteredForSeconds: GracePeriodSeconds * 20,
		TowerServing:         false,
		TowerNotServingWhy:   why,
	})

	if got.Coverable {
		t.Error("a channel with no session was reported as covered")
	}
	if !got.StillSettling {
		t.Error("a tower that cannot yet accept a session was reported as a " +
			"settled failure, which is what put a task in front of the user")
	}
	if strings.Contains(got.Reason, "the node has not negotiated") {
		t.Errorf("the verdict still blames the user's node: %q", got.Reason)
	}
	if !strings.Contains(got.Reason, "catching up") {
		t.Errorf("the verdict does not pass on the tower's own account: %q", got.Reason)
	}
	if !strings.Contains(got.Reason, "nothing for you to do") {
		t.Errorf("the verdict does not say the user has nothing to do: %q", got.Reason)
	}
}

// But a tower that is serving, past its grace period, with no session, is still
// the node's business — that is the case the message was written for.
func TestAServingTowerWithNoSessionStillNamesTheNode(t *testing.T) {
	t.Parallel()
	got := Assess(Inputs{
		ChanType:             store.ChanAnchors,
		TowerVersion:         ParseVersion("0.18.5-beta"),
		ClientVersion:        ParseVersion("0.18.5-beta"),
		RegisteredForSeconds: GracePeriodSeconds * 20,
		TowerServing:         true,
	})
	if got.StillSettling {
		t.Error("a serving tower with no session was excused as still settling")
	}
	if !strings.Contains(got.Reason, "has not negotiated one") {
		t.Errorf("the real case no longer says what is wrong: %q", got.Reason)
	}
}

// A caller that knows nothing about the tower's condition gets the old
// behaviour, rather than excusing every missing session forever.
func TestNoInformationAboutTheTowerDoesNotExcuseTheNode(t *testing.T) {
	t.Parallel()
	got := Assess(Inputs{
		ChanType:             store.ChanAnchors,
		TowerVersion:         ParseVersion("0.18.5-beta"),
		ClientVersion:        ParseVersion("0.18.5-beta"),
		RegisteredForSeconds: GracePeriodSeconds * 20,
		// TowerServing false and no reason: nothing was reported either way.
	})
	if got.StillSettling {
		t.Error("a missing session was excused on the strength of no evidence " +
			"at all, which would hide a genuine failure indefinitely")
	}
}
