package tower

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/paulscode/forktower/internal/store"
)

// Concern is something worth telling the user about a tower.
//
// Produced here and turned into alerts elsewhere, the same split the detection
// side uses: deciding what happened and deciding how loudly to say it are
// different jobs, and mixing them makes the first one untestable.
type Concern struct {
	Kind ConcernKind
	// ChannelID is set for concerns about one channel, zero for the tower.
	ChannelID int64
	TowerID   int64
	// Message is written for the user, not for a log reader.
	Message string
}

// ConcernKind is what sort of problem was found.
type ConcernKind string

// The concerns this monitor can raise.
const (
	// ConcernClientOff means the user's node is not backing up to anything.
	// The largest single onboarding obstacle, and not something Forktower can
	// fix on their behalf.
	ConcernClientOff ConcernKind = "tower.client_off"
	// ConcernNotRegistered means our tower exists and the user's node has never
	// heard of it.
	ConcernNotRegistered ConcernKind = "tower.not_registered"
	// ConcernChannelUncovered means a channel has no session that could protect
	// it — the silent, per-channel failure.
	ConcernChannelUncovered ConcernKind = "tower.channel_uncovered"
	// ConcernBackupsStalled means the state advanced and the backup count did
	// not. The classic watchtower failure.
	ConcernBackupsStalled ConcernKind = "tower.backups_stalled"
	// ConcernFeeRateFixed reports the rate baked into a session's justice
	// transactions when it looks low for the branch it would be spent on.
	ConcernFeeRateFixed ConcernKind = "tower.fee_rate_fixed"
	// ConcernSessionsExhausted means sessions have filled up and been replaced.
	// Not a fault; worth knowing because the replacements carry today's fee
	// rate, which is the only way that rate ever changes.
	ConcernSessionsExhausted ConcernKind = "tower.sessions_exhausted"
	// ConcernUnreachableFromNode means the registration is right, the tower is
	// serving, and no session has been agreed anyway — which leaves the path
	// between them.
	//
	// **Every other explanation has been ruled out by the time this fires.** The
	// node knows the tower, the tower is up and accepting, the grace period is
	// past, and there are channels wanting protection. What remains is that the
	// dial does not arrive, and neither end will say so: lnd logs the failure at
	// debug and reports the tower as an active session candidate regardless.
	// Measured on StartOS 0.4.0.1, where the node routed every connection through
	// Tor and the tower had only a local address — the proxy refused it, and for
	// eighty-seven minutes the only visible symptom was a number that stayed at
	// zero.
	ConcernUnreachableFromNode ConcernKind = "tower.unreachable_from_node"
)

// Monitor watches what one node is backing up to one tower.
type Monitor struct {
	client  ClientReader
	towerID int64
	// towerPubkey is our tower's identity, which is how it is recognised among
	// whatever else the user has registered.
	towerPubkey string
	// towerVersion is what our own tower runs, and decides which session types
	// it will accept.
	towerVersion Version
	// registeredForSeconds is how long the user's node has known about this
	// tower. Passed in rather than read from a clock so the decision stays pure.
	registeredForSeconds int64
	// towerServing and towerNotServingWhy are the tower's own readiness, so a
	// tower that cannot accept a session is not reported as a node that has not
	// asked for one.
	towerServing       bool
	towerNotServingWhy string
	// towerURI is the address a user would paste to register, carried here so a
	// concern that asks them to re-register can say what to re-register *with*.
	// Telling somebody to correct a registration without giving them the string
	// is most of the way to telling them nothing.
	towerURI string
	// canAttachOnion says this packaging can give the tower a Tor address on
	// request, which decides what the remedy is and is not the same everywhere.
	canAttachOnion bool
	// lowFeeSatPerVByte, when set, is the rate below which a session's baked-in
	// fee is worth mentioning.
	lowFeeSatPerVByte uint32
}

// MonitorOptions configures a Monitor.
type MonitorOptions struct {
	Client               ClientReader
	TowerID              int64
	TowerPubkey          string
	TowerVersion         Version
	RegisteredForSeconds int64
	LowFeeSatPerVByte    uint32
	// TowerServing and TowerNotServingWhy carry the tower's own readiness into
	// the coverage verdict, so a tower that cannot accept a session yet is not
	// reported as a node that has not asked for one.
	TowerServing       bool
	TowerNotServingWhy string
	// TowerURI is the address a user would paste to register with this tower.
	TowerURI string
	// CanAttachOnion says this packaging can give the tower a Tor address, and
	// that Forktower has asked it to when there is none.
	CanAttachOnion bool
}

// NewMonitor builds a Monitor.
func NewMonitor(opts MonitorOptions) (*Monitor, error) {
	if opts.Client == nil {
		return nil, errors.New("tower: a monitor needs a node to read")
	}
	if opts.TowerPubkey == "" {
		return nil, errors.New("tower: a monitor needs to know which tower it is watching")
	}
	return &Monitor{
		client:               opts.Client,
		towerID:              opts.TowerID,
		towerPubkey:          opts.TowerPubkey,
		towerVersion:         opts.TowerVersion,
		registeredForSeconds: opts.RegisteredForSeconds,
		towerServing:         opts.TowerServing,
		towerNotServingWhy:   opts.TowerNotServingWhy,
		towerURI:             opts.TowerURI,
		canAttachOnion:       opts.CanAttachOnion,
		lowFeeSatPerVByte:    opts.LowFeeSatPerVByte,
	}, nil
}

// Pass is one round of checking, for a set of channels.
type Pass struct {
	Coverage []store.Coverage
	Concerns []Concern
	Stats    ClientStats
	// ClientActive is false when the node's watchtower client is switched off,
	// in which case nothing is being backed up to any tower and the coverage
	// below is empty rather than negative.
	ClientActive bool
	// Registered is whether the node knows about our tower at all.
	Registered bool
	// Settled names the concerns this pass positively established do *not*
	// apply, as opposed to the ones it simply could not judge.
	//
	// **The distinction is what stops a restart stranding a warning.** A concern
	// withdrawn only when this process remembers raising it is never withdrawn
	// if the daemon restarts between the fault and its repair. Withdrawing it
	// whenever it is merely absent is worse: absence is also what "the tower is
	// still starting" and "the grace period has not passed" look like, and
	// retiring a real warning on that basis tells somebody their protection is
	// back when nothing was checked.
	Settled []ConcernKind
}

// Check looks at what the node is backing up and decides what it means for each
// channel.
//
// `previous` is the coverage recorded last time, used only to notice a backup
// count that has stopped moving. Nil on the first pass, which is correct: one
// observation cannot show something has stalled.
func (m *Monitor) Check(
	ctx context.Context, channels []store.Channel, previous []store.Coverage, now int64,
) (Pass, error) {
	pass := Pass{ClientActive: true}

	towers, err := m.client.Towers(ctx)
	switch {
	case errors.Is(err, ErrClientNotActive):
		// Not a failure to read — an answer. The node is up and is backing up to
		// nothing at all, which is a different and much larger problem than a
		// tower being unreachable, and the remedy is a setting on their node
		// that Forktower cannot change for them.
		pass.ClientActive = false
		pass.Concerns = append(pass.Concerns, Concern{
			Kind:    ConcernClientOff,
			TowerID: m.towerID,
			Message: "your Lightning node's watchtower client is switched off, so " +
				"none of your channels are being backed up to any tower. This is a " +
				"setting on your node and Forktower cannot change it for you.",
		})
		return pass, nil
	case err != nil:
		return Pass{}, fmt.Errorf("reading what your node is backing up: %w", err)
	}

	if stats, statsErr := m.client.Stats(ctx); statsErr == nil {
		pass.Stats = stats
		if stats.NumSessionsExh > 0 {
			pass.Concerns = append(pass.Concerns, Concern{
				Kind:    ConcernSessionsExhausted,
				TowerID: m.towerID,
				Message: fmt.Sprintf(
					"%d watchtower sessions have filled up and been replaced. That is "+
						"normal, and worth knowing: a replacement session takes the fee "+
						"rate your node is configured with today, which is the only way "+
						"the fee on a justice transaction ever changes.",
					stats.NumSessionsExh),
			})
		}
	}

	clientVersion, err := m.client.Version(ctx)
	if err != nil {
		return Pass{}, fmt.Errorf("reading your node's version: %w", err)
	}

	ours, found := findTower(towers, m.towerPubkey)
	pass.Registered = found
	if !found {
		pass.Concerns = append(pass.Concerns, Concern{
			Kind:    ConcernNotRegistered,
			TowerID: m.towerID,
			Message: "your Lightning node has not registered with this tower, so " +
				"nothing is being backed up to it.",
		})
	}

	held := sessionsByPolicy(ours)
	switch c, verdict := m.unreachableFromNode(found, held, channels); verdict {
	case registrationStale:
		pass.Concerns = append(pass.Concerns, c)
	case registrationGood:
		pass.Settled = append(pass.Settled, ConcernUnreachableFromNode)
	case registrationUnknown:
		// Nothing established. Whatever was said before stands.
	}
	pass.Coverage, pass.Concerns = m.assessChannels(
		channels, held, clientVersion, previous, now, pass.Concerns)
	pass.Concerns = append(pass.Concerns, m.feeConcerns(ours)...)
	return pass, nil
}

// unreachableFromNode reports the case where nothing is wrong at either end and
// there is still no session.
//
// The conditions are deliberately narrow, because this concern names a cause
// rather than describing a symptom and it must be right when it does. Every one
// of them removes a competing explanation: registered rules out the user not
// having done it, serving rules out the tower, the grace period rules out
// negotiation still being under way, and having channels rules out there being
// nothing to protect.
//
// **Zero sessions on an old registration means it has never once worked.** lnd
// keeps a session after it is negotiated — it does not drop them when a tower
// stops answering, which was checked on hardware while the tower's address was
// being changed underneath it. So no session here is not "the link is down
// today", it is "no channel state has ever reached this tower", and the message
// says so, because that is the fact that makes somebody act.
//
// The three-way answer matters as much as the concern. A session existing is
// positive evidence that the path works and is the only thing that retires this;
// everything else — a tower still starting, a node with no channels, a fresh
// registration — is "cannot tell", and must leave whatever was said standing.
func (m *Monitor) unreachableFromNode(
	registered bool, held map[PolicyType]policyFigures, channels []store.Channel,
) (Concern, registrationVerdict) {
	switch {
	case !registered, len(channels) == 0, !m.towerServing,
		m.registeredForSeconds < GracePeriodSeconds:
		return Concern{}, registrationUnknown
	case len(held) > 0:
		// Sessions exist, so the node has reached the tower. Nothing else this
		// concern could be about.
		return Concern{}, registrationGood
	}
	return Concern{
		Kind:    ConcernUnreachableFromNode,
		TowerID: m.towerID,
		Message: m.reRegisterMessage(),
	}, registrationStale
}

// reRegisterMessage explains why a registration that exists has to be made again.
//
// **Written against the reason it would otherwise be ignored.** Somebody reading
// this has already registered their node, remembers doing it, and can see the
// tower listed on their own node when they check — so "register your watchtower"
// reads as a mistake by Forktower and gets dismissed. It has to open by agreeing
// with them: the registration is there, it looks right, and it has still never
// carried a single backup. Only then does the instruction land.
//
// **The remedy differs by packaging and the first version of this did not.** It
// told everybody the tower needed a Tor address and that Forktower had asked for
// one on their behalf. That is true on StartOS 0.4.x, where the address the tower
// had was one a Tor-routed node cannot dial and the platform can attach an onion
// on request. On StartOS 0.3.5.1 and Umbrel it is false twice over: those nodes
// dial local addresses directly, and no such request is ever made — so it would
// have sent those users hunting for a screen that does not exist, which is worse
// than saying nothing.
func (m *Monitor) reRegisterMessage() string {
	const (
		lead = "your node is registered with this watchtower and the tower is " +
			"running, but not one channel state has ever reached it — no backup, " +
			"since the day you registered. Nothing on your node will tell you " +
			"this: it still lists the tower as usable and retries in silence."
		fault = " The registration itself is fine. What is wrong is the address " +
			"inside it: your node cannot open a connection to it."
		viaTor = " That is what happens when the node sends its connections " +
			"through Tor, which will not carry one to a local network address."
	)

	switch {
	case isOnionURI(m.towerURI):
		// An onion works from a node dialling directly and from one routed
		// through Tor, so this branch is the same advice on every platform.
		return lead + fault + viaTor + " The tower now has a Tor address, which " +
			"your node can reach. Re-register it with the new address — the old " +
			"one will not start working on its own:\n\n" +
			"    lncli wtclient remove " + m.towerPubkey + "\n" +
			"    lncli wtclient add " + m.towerURI + "\n\n" +
			"Nothing is lost by doing this: no backups were ever made under the " +
			"old registration, and your channels are protected from the moment " +
			"the first session is agreed, which takes a couple of minutes."

	case m.canAttachOnion:
		// No onion to hand them, so re-registering now would reproduce the same
		// failure. Ask for the address first.
		return lead + fault + viaTor + " The tower does not have a Tor address " +
			"yet, and Forktower cannot create one for it — that is done from the " +
			"Tor service, and Forktower has asked it for one on your behalf. Look " +
			"for that request, approve it, and re-register your node afterwards."

	default:
		// Nothing here can attach an onion, so there is no request to point at
		// and no new address to paste. Say what is known and no more: the address
		// is where the tower is, and the connection is not arriving anyway.
		return lead + fault + " The address it has is where the tower is, so " +
			"something between the two is stopping the connection. The usual cause " +
			"is the node being set to send every connection through Tor, which " +
			"will not carry one to a local address. Check that setting on your " +
			"node first; otherwise check that nothing between them is blocking " +
			"port " + portOf(m.towerURI) + "."
	}
}

// portOf is the port a client would dial, for a message that has to name it.
func portOf(uri string) string {
	if _, port, err := net.SplitHostPort(hostPortOf(uri)); err == nil && port != "" {
		return port
	}
	return "9911"
}

// isOnionURI says whether an address is one a Tor-routed node could dial.
func isOnionURI(uri string) bool {
	host := hostPortOf(uri)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(host)), ".onion")
}

// assessChannels decides coverage for each channel and collects what is wrong.
func (m *Monitor) assessChannels(
	channels []store.Channel, held map[PolicyType]policyFigures, clientVersion Version,
	previous []store.Coverage, now int64, concerns []Concern,
) ([]store.Coverage, []Concern) {
	present := make(map[PolicyType]bool, len(held))
	for policy := range held {
		present[policy] = true
	}

	was := make(map[int64]store.Coverage, len(previous))
	for _, c := range previous {
		was[c.ChannelID] = c
	}

	out := make([]store.Coverage, 0, len(channels))
	for _, ch := range channels {
		verdict := Assess(Inputs{
			ChanType:             ch.ChanType,
			TowerVersion:         m.towerVersion,
			ClientVersion:        clientVersion,
			SessionPolicies:      present,
			RegisteredForSeconds: m.registeredForSeconds,
			TowerServing:         m.towerServing,
			TowerNotServingWhy:   m.towerNotServingWhy,
		})

		figures := held[verdict.Policy]
		backups, feeRate := figures.backups, figures.feeSatPerKW
		cover := store.Coverage{
			ChannelID: ch.ID, TowerID: m.towerID,
			Coverable: verdict.Coverable, Reason: verdict.Reason,
			NumBackups: backups, CheckedAt: now,
		}
		if feeRate != nil {
			cover.SweepFeeSatPerKW = feeRate
		}
		if prior, ok := was[ch.ID]; ok && prior.LastBackupAt > 0 {
			cover.LastBackupAt = prior.LastBackupAt
		}
		if backups > 0 && (was[ch.ID].NumBackups != backups || cover.LastBackupAt == 0) {
			cover.LastBackupAt = now
		}
		out = append(out, cover)

		// A channel nobody can protect, said once per channel with its reason.
		// Not while the tower is still settling: sessions are not negotiated
		// instantly and a check that cries wolf on every fresh registration is
		// one people learn to ignore.
		if !verdict.Coverable && !verdict.StillSettling {
			concerns = append(concerns, Concern{
				Kind: ConcernChannelUncovered, ChannelID: ch.ID, TowerID: m.towerID,
				Message: fmt.Sprintf("channel %s is not protected by this tower: %s",
					shortOutpoint(ch), verdict.Reason),
			})
		}
	}
	return out, concerns
}

// feeConcerns reports a session whose baked-in fee looks low.
//
// Worth saying because nobody can change it after the fact: the justice
// transaction is pre-signed at each state update against the rate agreed when
// the session was created, and the tower holds no keys with which to bump it.
// The only remedy is negotiating fresh sessions, which is the user's action on
// their own node, so the message says so rather than implying we will handle it.
func (m *Monitor) feeConcerns(t *RegisteredTower) []Concern {
	if t == nil || m.lowFeeSatPerVByte == 0 {
		return nil
	}
	var out []Concern
	seen := map[PolicyType]bool{}
	for _, s := range t.Sessions {
		if s.SweepSatPerVByte == 0 || s.SweepSatPerVByte >= m.lowFeeSatPerVByte || seen[s.Policy] {
			continue
		}
		seen[s.Policy] = true
		out = append(out, Concern{
			Kind: ConcernFeeRateFixed, TowerID: m.towerID,
			Message: fmt.Sprintf(
				"the %s sessions with this tower pay %d sat/vB on a justice "+
					"transaction, which is below the %d sat/vB the other chain is "+
					"currently clearing. That rate was fixed when the session was "+
					"negotiated and is signed into every backup: nobody can raise it "+
					"afterwards. Re-registering with the tower negotiates fresh "+
					"sessions at your node's current rate.",
				s.Policy, s.SweepSatPerVByte, m.lowFeeSatPerVByte),
		})
	}
	return out
}

// StalledBackups reports channels whose backup count has not moved while their
// channel state has.
//
// Separate from Check because it needs a second observation: one look cannot
// show that something has stopped. The caller supplies the previous coverage and
// whether the channel's state actually advanced, because only the registry knows
// that — a quiet channel with a flat backup count is working exactly as intended.
//
// **The count is per session type, not per channel** — no Lightning node reports
// a per-channel figure. So this can over-report: two channels of the same type,
// one of them advancing, and a flat count will name both. That is the right
// direction to be wrong in. A tower that has quietly stopped taking backups is
// the failure this whole arm exists to catch, and naming one channel too many
// costs a second look while naming one too few costs the channel.
func (m *Monitor) StalledBackups(
	current, previous []store.Coverage, advanced map[int64]bool,
) []Concern {
	was := make(map[int64]store.Coverage, len(previous))
	for _, c := range previous {
		was[c.ChannelID] = c
	}

	var out []Concern
	for _, now := range current {
		prior, ok := was[now.ChannelID]
		if !ok || !advanced[now.ChannelID] {
			continue
		}
		if !now.Coverable || now.NumBackups != prior.NumBackups {
			continue
		}
		out = append(out, Concern{
			Kind: ConcernBackupsStalled, ChannelID: now.ChannelID, TowerID: m.towerID,
			Message: fmt.Sprintf(
				"channel %d has moved to a new state and the tower's backup count "+
					"for channels of its type has not changed (still %d). A tower "+
					"that is reachable but no longer receiving backups protects only "+
					"the states it already has.",
				now.ChannelID, now.NumBackups),
		})
	}
	return out
}

// findTower picks ours out of whatever the node has registered.
func findTower(towers []RegisteredTower, pubkey string) (*RegisteredTower, bool) {
	for i := range towers {
		if towers[i].Pubkey == pubkey {
			return &towers[i], true
		}
	}
	return nil, false
}

// policyFigures is what one session type is doing with a tower.
type policyFigures struct {
	backups int64
	// feeSatPerKW is the rate baked into this session's justice transactions,
	// converted from the sat/vB LND reports because sat/kW is the unit the
	// policy is actually expressed in. Nil when the tower did not say.
	feeSatPerKW *int32
}

// satPerVByteToSatPerKW converts between the two units LND uses for the same
// number. A weight unit is a quarter of a virtual byte, so a rate per vB is a
// quarter as much per weight unit — and per *kilo*-weight, 250 times as much.
const satPerVByteToSatPerKW = 250

// sessionsByPolicy is what the node actually holds with a tower, per session
// type.
//
// Evidence, and worth more than any inference from version numbers: a session of
// a given type existing means the tower accepted it. Backups are summed across
// sessions of the same type, because a session that filled up and was replaced
// still holds the states it took.
func sessionsByPolicy(t *RegisteredTower) map[PolicyType]policyFigures {
	out := map[PolicyType]policyFigures{}
	if t == nil {
		return out
	}
	for _, s := range t.Sessions {
		if s.Policy == PolicyUnknown {
			continue
		}
		f := out[s.Policy]
		f.backups += int64(s.NumBackups)
		if f.feeSatPerKW == nil && s.SweepSatPerVByte > 0 {
			//nolint:gosec // a fee rate in sat/vB is nowhere near a 32-bit bound
			rate := int32(s.SweepSatPerVByte) * satPerVByteToSatPerKW
			f.feeSatPerKW = &rate
		}
		out[s.Policy] = f
	}
	return out
}

// shortOutpoint names a channel the way the rest of the interface does.
func shortOutpoint(c store.Channel) string {
	const shortTxIDLen = 8
	txid := c.FundingTxID
	if len(txid) > shortTxIDLen {
		txid = txid[:shortTxIDLen] + "…"
	}
	return fmt.Sprintf("%s:%d", txid, c.FundingVout)
}
