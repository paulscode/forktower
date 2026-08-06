package tower

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/paulscode/forktower/internal/store"
)

// PolicyType is a watchtower session type. One channel needs a session of
// exactly one of these, and a tower that does not offer it covers that channel
// not at all.
//
// LND names these `legacy`, `anchor` and `taproot` over the wire, and derives
// which one a channel needs from the channel's own commitment type. There is no
// negotiation and no fallback: a channel whose type has no matching session is
// refused at session creation.
type PolicyType string

// The session types LND offers.
const (
	PolicyLegacy  PolicyType = "legacy"
	PolicyAnchor  PolicyType = "anchor"
	PolicyTaproot PolicyType = "taproot"
	// PolicyUnknown means the channel's commitment type is not known, so neither
	// is the session it would need.
	PolicyUnknown PolicyType = "unknown"
)

// PolicyForChannel is which session type a channel needs.
//
// Mirrors LND's own `TypeFromChannel`. A channel whose type we could not read is
// deliberately *not* mapped to legacy: guessing would produce a confident
// "covered" for a channel nobody has checked, and being told "we cannot tell" is
// worth more than being told the wrong thing calmly.
func PolicyForChannel(t store.ChanType) PolicyType {
	switch t {
	case store.ChanTaproot:
		return PolicyTaproot
	case store.ChanAnchors:
		return PolicyAnchor
	case store.ChanLegacy, store.ChanStaticRemote:
		return PolicyLegacy
	case store.ChanTypeUnknown:
		return PolicyUnknown
	default:
		return PolicyUnknown
	}
}

// Version is an LND version, compared rather than displayed.
type Version struct {
	Major, Minor, Patch int
	// Raw is what was actually reported, kept for the reason strings — a user
	// checking our arithmetic needs the number we were given.
	Raw string
	// Known is false when the version could not be read. Everything downstream
	// treats that as "cannot say" rather than as any particular version.
	Known bool
}

// ParseVersion reads an LND version string.
//
// LND reports things like `0.18.5-beta commit=v0.18.5-beta`. Only the first
// three numbers matter; the suffix and the commit are noise for this purpose.
func ParseVersion(raw string) Version {
	v := Version{Raw: strings.TrimSpace(raw)}
	if v.Raw == "" {
		return v
	}
	head := strings.TrimPrefix(strings.Fields(v.Raw)[0], "v")
	// Cut anything after the numbers: `-beta`, `-rc1`, `+build`.
	for i, r := range head {
		if (r < '0' || r > '9') && r != '.' {
			head = head[:i]
			break
		}
	}
	parts := strings.Split(head, ".")
	if len(parts) < 2 {
		return v
	}
	nums := make([]int, 3)
	for i := range 3 {
		if i >= len(parts) || parts[i] == "" {
			continue
		}
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return v
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	v.Known = true
	return v
}

// atLeast reports whether v is at or above the given version.
func (v Version) atLeast(major, minor int) bool {
	if !v.Known {
		return false
	}
	if v.Major != major {
		return v.Major > major
	}
	return v.Minor >= minor
}

// String renders the version as reported, or a phrase saying it is unknown.
func (v Version) String() string {
	if !v.Known {
		return "an unknown version"
	}
	return "v" + v.Raw
}

// minVersion is the earliest LND that can do something.
type minVersion struct{ major, minor int }

// firstVersionFor is the LND release that introduced each session type, on both
// the client and the tower side.
//
// Measured from the tagged source rather than from release notes: `legacy` and
// `reward` from the beginning, `anchor` from v0.12.0, `taproot` from v0.18.0.
// v0.21.0 adds a *separate* taproot-final type, which is not modelled here
// because no channel can require it without an explicit opt-in that current
// releases still gate behind a flag.
func firstVersionFor(p PolicyType) (minVersion, bool) {
	switch p {
	case PolicyLegacy:
		// From the beginning, so any version that can be read at all will do.
		return minVersion{}, true
	case PolicyAnchor:
		return minVersion{0, 12}, true
	case PolicyTaproot:
		return minVersion{0, 18}, true
	case PolicyUnknown:
		return minVersion{}, false
	default:
		return minVersion{}, false
	}
}

// Supports reports whether an LND at this version can handle a session type,
// on either side of the connection.
func Supports(v Version, p PolicyType) bool {
	earliest, ok := firstVersionFor(p)
	if !ok {
		return false
	}
	if earliest == (minVersion{}) {
		// Available in every release, so the only question is whether we could
		// read a version at all. An unreadable one still supports nothing:
		// guessing is how a channel gets reported protected by a session
		// nothing could create.
		return v.Known
	}
	return v.atLeast(earliest.major, earliest.minor)
}

// Inputs are everything the coverage decision is allowed to look at.
type Inputs struct {
	// ChanType is the channel's commitment type, which decides the session it
	// needs.
	ChanType store.ChanType
	// TowerVersion is the LND our companion tower runs. ClientVersion is the LND
	// the user runs, which is the side that creates sessions.
	TowerVersion  Version
	ClientVersion Version
	// SessionPolicies are the session types the user's node actually holds with
	// this tower. Evidence, and worth more than the version arithmetic: a
	// session of the right type existing means the tower accepted it.
	SessionPolicies map[PolicyType]bool
	// RegisteredFor is how long the tower has been registered. Sessions are not
	// negotiated instantly, so a tower registered moments ago with no sessions
	// is not yet a tower that has failed.
	RegisteredForSeconds int64
}

// GracePeriodSeconds is how long after registration a tower with no sessions is
// given the benefit of the doubt.
//
// LND's session negotiator uses exponential backoff, and a tower added after the
// client has already started can sit in that backoff for minutes: measured at
// 2m30s in the worst observed case, during which the client reports zero
// sessions and zero backups and looks exactly like a tower that is broken. Ten
// minutes is comfortably past that, and a readiness check that cries wolf on
// every fresh registration is one people learn to ignore.
const GracePeriodSeconds = 600

// Verdict is whether a tower covers a channel, and why.
type Verdict struct {
	Coverable bool
	// Reason is filled in both directions. A refusal with no reason is an
	// accusation without evidence; a "yes" with no reason gives a reader nothing
	// to check.
	Reason string
	// Policy is the session type this channel needs.
	Policy PolicyType
	// StillSettling means there is no session yet but the tower has not been
	// registered long enough for that to mean anything. Not coverable, but not a
	// fault either, and the two must not read the same.
	StillSettling bool
}

// Assess decides whether one tower covers one channel.
//
// Pure: no storage, no clock, no network. This decides whether a user is told
// their money is protected, so it is the part that has to be checkable by
// reading.
//
// **The order of the checks is the point.** An existing session is evidence and
// is trusted over any inference from version numbers; only when there is no
// session does this reach for the versions, and then only to explain *why*. A
// coverage check that reasoned purely from versions would confidently report a
// channel protected by a session that was never negotiated.
func Assess(in Inputs) Verdict {
	policy := PolicyForChannel(in.ChanType)

	if policy == PolicyUnknown {
		return Verdict{
			Policy: policy,
			Reason: "the channel's commitment type is not known, so it cannot be " +
				"said which kind of tower session it needs — treated as not covered " +
				"rather than assumed to be fine",
		}
	}

	if in.SessionPolicies[policy] {
		return Verdict{
			Coverable: true,
			Policy:    policy,
			Reason: fmt.Sprintf(
				"the node holds a %s session with this tower, which is the kind a "+
					"%s channel needs", policy, in.ChanType),
		}
	}

	// No session. Say which end cannot do it, because the remedy differs: one is
	// upgrading the user's node, the other is upgrading ours.
	switch {
	case !Supports(in.ClientVersion, policy):
		return Verdict{
			Policy: policy,
			Reason: fmt.Sprintf(
				"a %s channel needs a %s tower session, and the Lightning node is "+
					"running %s, which cannot make one",
				in.ChanType, policy, in.ClientVersion),
		}
	case !Supports(in.TowerVersion, policy):
		return Verdict{
			Policy: policy,
			Reason: fmt.Sprintf(
				"a %s channel needs a %s tower session, and the tower is running %s, "+
					"which does not accept one",
				in.ChanType, policy, in.TowerVersion),
		}
	case in.RegisteredForSeconds < GracePeriodSeconds:
		return Verdict{
			Policy:        policy,
			StillSettling: true,
			Reason: fmt.Sprintf(
				"no %s session yet, but the tower was registered less than %d "+
					"minutes ago and sessions are not negotiated immediately",
				policy, GracePeriodSeconds/60),
		}
	default:
		return Verdict{
			Policy: policy,
			Reason: fmt.Sprintf(
				"both ends support %s sessions but the node has not negotiated one "+
					"with this tower, so this channel is not being backed up",
				policy),
		}
	}
}
