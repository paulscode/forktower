package tower

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"strings"
	"time"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/store"
	"github.com/paulscode/forktower/internal/words"
)

// ConcernExternalOnly means every tower protecting the user is somebody else's.
//
// Not a fault, and not something to be alarmed about — an external tower is a
// real tower and registering with several is good advice. It is worth *saying*,
// because what can honestly be promised about a tower nobody here runs is
// different: it cannot be restarted from this machine, its configuration is not
// ours, and if it stops the remedy is to register with another.
const ConcernExternalOnly ConcernKind = "tower.external_only"

// ConcernOursNotRegistered means the node backs up to somebody else's tower and
// not to the one this installation runs.
//
// **The gap this whole program exists to close, left open.** A tower somebody
// else runs watches whichever chain its operator's node follows — which cannot
// be seen from here, cannot be verified, and changes without notice when they
// upgrade. It may happen to be watching the chain the user's own node cannot
// see. It may be watching the same chain the user's node already watches, in
// which case it duplicates protection they already have and nothing at all is
// covering the other one.
//
// The tower here is the one with a known view of that chain, and the one whose
// backups can be checked channel by channel. So this is worth saying plainly
// rather than describing the deployment and leaving the user to infer it.
const ConcernOursNotRegistered ConcernKind = "tower.ours_not_registered"

// ConcernRegistrationStale means the node has our tower registered at an address
// it can no longer reach.
//
// **The failure this was written for looks exactly like success.** A node holds
// the address it was given when the user registered, and lnd resolves a name to
// an address at that moment and stores the number — measured, not assumed. When
// the container is rebuilt it comes back on a different address and the stored
// one points at nothing. The registration is still listed, the tower is still
// running and still reports itself healthy, and every session request dials a
// dead address and fails silently: lnd logs the failures at debug, so at the
// stock level it says "Requesting new session." once and then nothing, forever.
//
// Two machines sat like that for hours with nothing anywhere saying so. Whoever
// was looking would have seen a registered tower, a healthy tower, and no
// protection at all.
const ConcernRegistrationStale ConcernKind = "tower.registration_stale"

// Scout records the watchtowers the user's own node is using.
//
// **Discovered rather than configured, and that is the point.** The node already
// knows which towers it backs up to — the user typed them in when they
// registered — so asking them to type the same list into Forktower would be
// asking twice and getting it wrong once. Anything the node reports that this
// installation did not start is recorded as somebody else's.
//
// What can be known about an external tower is exactly what the user's node
// knows: whether sessions exist, how many states have gone across, and at what
// fee rate. There is no separate API to ask, because a watchtower's own
// interface speaks the tower protocol and answers only to clients. That
// limitation is also the honest framing — "is my protection working" is a better
// question than "is that machine up", and only the node can answer it.
type Scout struct {
	store    ScoutStore
	client   ClientReader
	bus      *bus.Bus
	log      *slog.Logger
	interval time.Duration
	now      func() time.Time
	// runsOwn says this installation brings up a watchtower of its own, which
	// changes the advice completely: the answer to "every tower here is somebody
	// else's" is to register ours, not to shrug.
	runsOwn bool

	// announced remembers what has been said, so a standing state is not repeated
	// every pass.
	announced map[string]store.TowerStatus
	// firstSeen is when each external tower was first recorded, which is what the
	// registration grace period is measured from.
	firstSeen map[string]int64

	// alsoReachableAt are the other addresses our tower answers on, beyond the
	// one it advertises. See TowerInstance.AlsoReachableAt for why the difference
	// matters more than it sounds.
	alsoReachableAt []string

	// resolve turns the name our tower advertises into the addresses a client
	// would have stored for it.
	//
	// **Injected because the comparison is worthless without it.** lnd does not
	// keep the name it was given; it resolves it when the tower is added and
	// stores the result. So a node registered against `forktower.startos:9911`
	// reports `10.0.3.52:9911`, and a string comparison against what we advertise
	// calls a perfectly good registration stale. Crying wolf here is worse than
	// silence — this concern's whole value is that it is believed.
	resolve func(ctx context.Context, host string) ([]string, error)
}

// ScoutStore is the storage this reads and writes.
type ScoutStore interface {
	UpsertTower(ctx context.Context, t store.Tower) (int64, bool, error)
	SetTowerStatus(ctx context.Context, id int64, h store.TowerHealth, now int64) error
	ListTowers(ctx context.Context, f store.TowerFilter) ([]store.Tower, error)
	UpsertCoverage(ctx context.Context, c store.Coverage) error
	ListChannels(ctx context.Context, f store.ChannelFilter) ([]store.Channel, error)
}

// ScoutOptions configures a Scout.
type ScoutOptions struct {
	Store    ScoutStore
	Client   ClientReader
	Bus      *bus.Bus
	Log      *slog.Logger
	Interval time.Duration
	Now      func() time.Time
	// RunsOwnWatchtower says this installation brings up a watchtower of its own.
	RunsOwnWatchtower bool
	// AlsoReachableAt are the other addresses our own tower answers on.
	AlsoReachableAt []string
	// Resolve looks up a hostname. Defaults to the system resolver; tests supply
	// their own rather than depending on what the build machine's DNS says.
	Resolve func(ctx context.Context, host string) ([]string, error)
}

// NewScout builds a Scout.
func NewScout(opts ScoutOptions) (*Scout, error) {
	if opts.Store == nil || opts.Bus == nil {
		return nil, errors.New("tower: a scout needs storage and a bus")
	}
	if opts.Client == nil {
		return nil, errors.New("tower: a scout needs a node to read")
	}
	if opts.Log == nil {
		opts.Log = slog.New(discardHandler{})
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Resolve == nil {
		opts.Resolve = net.DefaultResolver.LookupHost
	}
	return &Scout{
		store: opts.Store, client: opts.Client, bus: opts.Bus, log: opts.Log,
		interval: opts.Interval, now: opts.Now, runsOwn: opts.RunsOwnWatchtower,
		announced:       map[string]store.TowerStatus{},
		firstSeen:       map[string]int64{},
		resolve:         opts.Resolve,
		alsoReachableAt: opts.AlsoReachableAt,
	}, nil
}

// Run watches until the context is cancelled.
func (s *Scout) Run(ctx context.Context) error {
	s.pass(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.pass(ctx)
		}
	}
}

// pass records what the node is backing up to.
func (s *Scout) pass(ctx context.Context) {
	registered, err := s.client.Towers(ctx)
	switch {
	case errors.Is(err, ErrClientNotActive):
		// Reported by the warden, which has the whole picture. Saying it twice
		// would be two notifications for one fact.
		return
	case err != nil:
		s.log.Error("reading which watchtowers your node uses",
			slog.String("error", err.Error()))
		return
	}

	channels, err := s.store.ListChannels(ctx, store.ChannelFilter{OpenOnly: true})
	if err != nil {
		s.log.Error("reading your channels", slog.String("error", err.Error()))
		return
	}

	clientVersion, err := s.client.Version(ctx)
	if err != nil {
		s.log.Error("reading your node's version", slog.String("error", err.Error()))
		return
	}

	// Which towers this installation runs, read fresh each pass rather than
	// configured.
	//
	// **It cannot be known in advance.** A tower's identity comes from the tower,
	// and it has not said one until it has answered — so a scout told a pubkey at
	// construction would be told nothing useful. Read from storage, it is right as
	// soon as the warden has recorded it, and stays right across a restart.
	//
	// Getting this wrong is not a cosmetic matter: recording our own tower as
	// somebody else's would overwrite the flag the page uses to decide whether it
	// can honestly offer to restart it.
	ours, err := s.managed(ctx)
	if err != nil {
		s.log.Error("reading which watchtowers this installation runs",
			slog.String("error", err.Error()))
		return
	}

	now := s.now().Unix()
	var external, oursRegistered int
	var stale []staleRegistration
	var anyGood bool
	for i := range registered {
		t := registered[i]
		if t.Pubkey == "" {
			continue
		}
		if uri, mine := ours[t.Pubkey]; mine {
			oursRegistered++
			// **Ours, and therefore worth looking at rather than counting.** This
			// used to `continue` here, which meant the one place that could have
			// noticed a registration pointing at a dead address skipped it by
			// construction.
			switch bad, verdict := s.checkRegistration(ctx, t, uri); verdict {
			case registrationStale:
				stale = append(stale, bad)
			case registrationGood:
				anyGood = true
			case registrationUnknown:
				// Nothing learned this pass. Deliberately not counted as either.
			}
			continue
		}
		external++
		s.record(ctx, t, channels, clientVersion, now)
	}

	s.describeShape(external, oursRegistered, len(ours))
	s.describeRegistrations(stale, anyGood)
}

// describeShape says what this deployment's watchtower arrangement leaves open.
//
// **The distinction that was missing is "not yet" against "not at all".** A
// tower this installation runs is not recorded until it has answered and
// reported its own pubkey — lnd takes a while to open that listener — so for the
// first minutes of every installation the honest answer to "does this deployment
// run a tower" is "ask again shortly". Reading that as "no" told a user with a
// third-party tower that their arrangement was external-only and that it worked,
// once, permanently, while the tower they should have been registering was
// starting up underneath.
func (s *Scout) describeShape(external, oursRegistered, oursKnown int) {
	switch {
	case s.runsOwn && oursKnown == 0:
		// Ours has not said who it is yet. Nothing can be concluded about the
		// shape of this deployment until it has, and concluding anyway is the
		// bug this exists to prevent.
		return

	case s.runsOwn && oursRegistered > 0:
		// Registered with ours. Whatever else they run alongside it is their
		// business and a good idea.
		s.clearConcern(keyOursNotRegistered, ConcernOursNotRegistered)

	case s.runsOwn:
		s.announceOursNotRegistered(external)

	case external > 0:
		s.announceExternalOnly(external)
	}
}

// managed is the towers this installation runs, by pubkey, with the address each
// one currently advertises.
//
// The address comes along because the pubkey alone cannot answer "is this
// registration still good" — the pubkey is what never changes, and the address is
// the part that rots.
func (s *Scout) managed(ctx context.Context) (map[string]string, error) {
	rows, err := s.store.ListTowers(ctx, store.TowerFilter{ManagedOnly: true})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, t := range rows {
		out[t.Pubkey] = t.URI
	}
	return out, nil
}

// staleRegistration is one of our towers that the node cannot reach at the
// address it holds.
type staleRegistration struct {
	Pubkey string
	// Held is what the node has stored, and Want is where the tower actually is.
	Held []string
	Want string
}

// registrationVerdict is what one pass could establish about a registration.
//
// **Three states, because two was wrong.** The first version answered yes or no,
// and every "cannot tell" came back as "no" — which withdrew a standing warning
// the moment a name failed to resolve. Caught on hardware: a real stale
// registration was reported and then retracted while it was still stale, leaving
// the user with a resolved notification and no protection. "I could not tell" and
// "it is fixed" must not produce the same output; that is the same defect as the
// warden's `lastConcerns` in a new place.
type registrationVerdict int

const (
	registrationUnknown registrationVerdict = iota
	registrationGood
	registrationStale
)

// checkRegistration asks whether the node could still reach our tower at the
// address it has stored for it.
//
// **A live session outranks the address comparison.** The comparison is an
// inference — these strings do not match, therefore the dial cannot arrive — and
// a session is evidence that it does, which the rest of this package already
// prefers wherever the two are available. It also covers the case the inference
// gets wrong: an address that still works after Forktower has stopped
// advertising it.
func (s *Scout) checkRegistration(
	ctx context.Context, t RegisteredTower, uri string,
) (staleRegistration, registrationVerdict) {
	if len(t.Sessions) > 0 {
		return staleRegistration{}, registrationGood
	}

	want := hostPortOf(uri)
	if want == "" || len(t.Addresses) == 0 {
		return staleRegistration{}, registrationUnknown
	}

	reachable, ok := s.addressesFor(ctx, want)
	if !ok {
		return staleRegistration{}, registrationUnknown
	}
	for _, held := range t.Addresses {
		if reachable[strings.TrimSpace(held)] {
			// At least one address the node holds still arrives. A dead one
			// alongside it is untidy and costs a retry, not protection.
			return staleRegistration{}, registrationGood
		}
	}
	return staleRegistration{
		Pubkey: t.Pubkey, Held: t.Addresses, Want: want,
	}, registrationStale
}

// addressesFor is every host:port a client could hold for our tower and still
// reach it. The bool is false when that set cannot be determined.
//
// A name resolves to numbers, and the numbers are what a client stores, so both
// belong in the set. When the name will not resolve the set is unknowable and the
// caller must not guess.
func (s *Scout) addressesFor(ctx context.Context, want string) (map[string]bool, bool) {
	out := map[string]bool{}
	// Every address the tower answers on, not only the one it advertises. A node
	// registered against the sibling hostname before an onion existed is still
	// reaching the tower, and is not to be told otherwise.
	for _, addr := range append([]string{want}, s.alsoReachableAt...) {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		out[hostPortOf(addr)] = true
	}

	// A name has to be resolved as well as matched, because lnd stores what the
	// name resolved to rather than the name. Failing to resolve *any* of them
	// leaves the answer unknown rather than partial: a half-built set would call
	// a good registration stale.
	for addr := range maps.Clone(out) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			continue
		}
		if net.ParseIP(host) != nil || strings.HasSuffix(strings.ToLower(host), ".onion") {
			// Already a number, or an onion, which lnd stores verbatim — measured
			// on hardware, where a tower added as an onion was read back as that
			// onion.
			continue
		}
		lookupCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
		addrs, err := s.resolve(lookupCtx, host)
		cancel()
		if err != nil || len(addrs) == 0 {
			return nil, false
		}
		for _, a := range addrs {
			out[net.JoinHostPort(a, port)] = true
		}
	}
	return out, true
}

// resolveTimeout bounds a name lookup. A resolver that hangs must not stall the
// pass that reports on every other tower.
const resolveTimeout = 5 * time.Second

// hostPortOf strips the identity key from a URI, leaving the part a client dials.
func hostPortOf(uri string) string {
	if _, after, found := strings.Cut(uri, "@"); found {
		return strings.TrimSpace(after)
	}
	return strings.TrimSpace(uri)
}

// describeRegistrations says that a registration has gone stale, and withdraws it
// once it has been corrected.
//
// Withdrawal needs a positive finding, not merely the absence of a negative one.
// A pass that learned nothing leaves whatever was said standing, because telling
// somebody their protection is back when nothing was checked is worse than
// saying nothing at all.
func (s *Scout) describeRegistrations(stale []staleRegistration, anyGood bool) {
	if len(stale) == 0 {
		if !anyGood {
			// Nothing was learned this pass. Whatever was said stands.
			return
		}
		// **Declared, not diffed.** Withdrawing only on the transition from bad to
		// good means never withdrawing at all when the daemon restarts in between:
		// the memory of having raised it is gone and the warning outlives the
		// process that raised it. Declared once per start instead — enough to
		// retire a stranded warning, and not a bus event a minute otherwise — and
		// dropped by the alert layer when there is nothing standing.
		delete(s.announced, keyRegistrationStale)
		if s.announced[keyRegistrationSettled] != "" {
			return
		}
		s.announced[keyRegistrationSettled] = store.TowerReachable
		s.bus.Publish(bus.TowerConcern{
			Concern: string(ConcernRegistrationStale), Cleared: true,
		})
		return
	}
	if s.announced[keyRegistrationStale] != "" {
		return
	}
	// Raised again, so the next settling is news again.
	delete(s.announced, keyRegistrationSettled)
	s.announced[keyRegistrationStale] = store.TowerReachable

	bad := stale[0]
	// **Written for somebody who is sure they have already done this.** They
	// registered the tower, they remember it, and it is still listed on their own
	// node — so an instruction to register it reads as Forktower being wrong, and
	// gets ignored by precisely the person who needs it. So: agree that they did
	// it, say what changed underneath them, and be specific that it was not their
	// mistake.
	s.bus.Publish(bus.TowerConcern{
		Concern: string(ConcernRegistrationStale),
		Message: fmt.Sprintf(
			"you registered your node with Forktower's watchtower, and that "+
				"registration was correct when you made it. The tower has since moved "+
				"to a different address — this happens when the app is reinstalled or "+
				"updated — and your node still has the old one: %s. It is no longer "+
				"reachable, so no backup is arriving and no channel here is protected. "+
				"Nothing on your node will tell you this: it retries the old address "+
				"silently and goes on listing the tower as usable.\n\n"+
				"Point it at the address the tower is on now. The old registration will "+
				"not start working again on its own:\n\n"+
				"    lncli wtclient remove %s\n"+
				"    lncli wtclient add %s@%s\n\n"+
				"Your channels are protected again a couple of minutes after the first "+
				"session is agreed.",
			plainList(bad.Held), bad.Pubkey, bad.Pubkey, bad.Want),
	})
}

// plainList reads a set of addresses as a person would say them.
func plainList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}

// record files one external tower and what it protects.
func (s *Scout) record(
	ctx context.Context, t RegisteredTower, channels []store.Channel,
	clientVersion Version, now int64,
) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()

	first, seen := s.firstSeen[t.Pubkey]
	if !seen {
		first = now
	}

	id, _, err := s.store.UpsertTower(writeCtx, store.Tower{
		Kind: store.TowerLND, Pubkey: t.Pubkey, URI: addressOf(t),
		// Not ours. The dashboard says so, because what may honestly be promised
		// about somebody else's machine is different.
		Managed: false, FirstSeenAt: first, UpdatedAt: now,
	})
	if err != nil {
		s.log.Error("recording a watchtower your node uses",
			slog.String("error", err.Error()))
		return
	}
	if !seen {
		s.firstSeen[t.Pubkey] = s.firstSeenOf(writeCtx, id, now)
	}

	held := sessionsByPolicy(&t)
	present := make(map[PolicyType]bool, len(held))
	for policy := range held {
		present[policy] = true
	}

	health := store.TowerHealth{Status: store.TowerReachable}
	if len(held) == 0 {
		health.Status = store.TowerTemporarilyUnreachable
		health.Detail = "your node has registered with this tower but has not " +
			"agreed a session with it yet"
	}
	if err := s.store.SetTowerStatus(writeCtx, id, health, now); err != nil {
		s.log.Error("recording a watchtower's condition", slog.String("error", err.Error()))
		return
	}
	s.announceHealth(t.Pubkey, id, health)

	for _, ch := range channels {
		verdict := Assess(Inputs{
			ChanType: ch.ChanType,
			// **Unknown, and honestly so.** An external tower's version cannot be
			// read: there is no interface to ask. Coverage therefore rests entirely
			// on whether a session of the right kind exists, which is evidence
			// rather than inference and is the better basis anyway.
			TowerVersion:         Version{},
			ClientVersion:        clientVersion,
			SessionPolicies:      present,
			RegisteredForSeconds: now - s.firstSeen[t.Pubkey],
		})
		figures := held[verdict.Policy]
		cover := store.Coverage{
			ChannelID: ch.ID, TowerID: id,
			Coverable: verdict.Coverable, Reason: verdict.Reason,
			NumBackups: figures.backups, CheckedAt: now,
		}
		if figures.feeSatPerKW != nil {
			cover.SweepFeeSatPerKW = figures.feeSatPerKW
		}
		if err := s.store.UpsertCoverage(writeCtx, cover); err != nil {
			s.log.Error("recording what a watchtower protects",
				slog.String("error", err.Error()))
			return
		}
	}
}

// firstSeenOf reads back when a tower was first recorded, so that a restart does
// not restart its grace period.
func (s *Scout) firstSeenOf(ctx context.Context, id, now int64) int64 {
	rows, err := s.store.ListTowers(ctx, store.TowerFilter{})
	if err != nil {
		return now
	}
	for _, t := range rows {
		if t.ID == id && t.FirstSeenAt > 0 {
			return t.FirstSeenAt
		}
	}
	return now
}

// announceHealth publishes a change, and only a change.
func (s *Scout) announceHealth(pubkey string, id int64, health store.TowerHealth) {
	if s.announced[pubkey] == health.Status {
		return
	}
	previous := s.announced[pubkey]
	s.announced[pubkey] = health.Status
	if previous == "" {
		previous = store.TowerStatusUnknown
	}
	s.bus.Publish(bus.TowerHealthChanged{
		TowerID: id, TowerKind: string(store.TowerLND), Pubkey: pubkey,
		Managed: false, Status: string(health.Status), Detail: health.Detail,
		Previous: string(previous),
	})
}

// Keys under which the scout remembers what it has already said.
const (
	keyExternalOnly      = "external-only"
	keyOursNotRegistered = "ours-not-registered"
	keyRegistrationStale = "registration-stale"
	// Separate from the key above: one records having warned, the other having
	// declared the warning spent. Sharing a key would make the second withdraw
	// the memory of the first.
	keyRegistrationSettled = "registration-settled"
)

// announceOursNotRegistered asks for the one registration that closes the gap.
//
// Said once while it stands, and withdrawn when it is done — the user has to go
// to their own node to act on this, and coming back to the same sentence with
// nothing to say whether it took is the complaint that produced half of 0.6.2.
func (s *Scout) announceOursNotRegistered(external int) {
	if s.announced[keyOursNotRegistered] != "" {
		return
	}
	s.announced[keyOursNotRegistered] = store.TowerReachable

	message := "Forktower runs a watchtower here with a view of " + words.OtherChain +
		", and your node is not registered with it. That is the one registration " +
		"this protection depends on, and Forktower cannot make it for you. Open " +
		"Forktower for the address and the steps."
	if external > 0 {
		message = "your node is backing up to " +
			plainCount(external, "a watchtower", "watchtowers") +
			" that Forktower does not run, and not to the one it runs here. " +
			"Keep the one you have — but a tower somebody else runs watches " +
			"whichever chain their node follows, which cannot be seen from here " +
			"and can change without notice. The tower here is the one with a " +
			"known view of " + words.OtherChain + ", which is the chain your own " +
			"node cannot see. Open Forktower for the address and the steps."
	}

	s.bus.Publish(bus.TowerConcern{
		Concern: string(ConcernOursNotRegistered),
		Message: message,
	})
}

// clearConcern withdraws something previously said, once.
func (s *Scout) clearConcern(key string, kind ConcernKind) {
	if s.announced[key] == "" {
		return
	}
	delete(s.announced, key)
	s.bus.Publish(bus.TowerConcern{Concern: string(kind), Cleared: true})
}

// announceExternalOnly says once that every tower protecting the user belongs to
// somebody else.
//
// Said at all because it changes what can be done when one stops: there is no
// process here to restart and no configuration here to correct, and the remedy
// is to register with another. Said *once*, because it is a description of the
// deployment rather than a problem with it.
func (s *Scout) announceExternalOnly(count int) {
	if s.announced[keyExternalOnly] != "" {
		return
	}
	s.announced[keyExternalOnly] = store.TowerReachable

	s.bus.Publish(bus.TowerConcern{
		Concern: string(ConcernExternalOnly),
		// **It used to open with "That works."** It does work, for the thing a
		// watchtower ordinarily does. What it cannot be relied on for is the
		// thing this program is about: a tower somebody else runs watches
		// whichever chain their node follows, and that is neither visible from
		// here nor fixed. Leading with reassurance talked people out of the one
		// step that closes the gap.
		Message: fmt.Sprintf(
			"your channels are protected by %s that Forktower does not run, and "+
				"this installation runs none of its own. Registering with more than "+
				"one is sensible, and there is nothing wrong with the towers you "+
				"have — but a tower somebody else runs watches whichever chain their "+
				"node follows. That cannot be seen from here and can change when they "+
				"upgrade, so whether %s is being watched at all is not something "+
				"Forktower can tell you. If one stops there is also nothing here to "+
				"restart and no settings here to correct, so the remedy would be to "+
				"register with another.",
			plainCount(count, "a watchtower", "watchtowers"), words.OtherChain),
	})
}

// plainCount reads as a person would say it.
func plainCount(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// addressOf is where a tower can be reached, as the node reports it.
func addressOf(t RegisteredTower) string {
	if len(t.Addresses) == 0 {
		return ""
	}
	return t.Pubkey + "@" + t.Addresses[0]
}
