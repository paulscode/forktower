package alert

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/chainview"
	"github.com/paulscode/forktower/internal/config"
	"github.com/paulscode/forktower/internal/store"
)

// openTempStore is a throwaway database for tests that only need one to exist.
func openTempStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "forktower.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// The five tiers rank onto the three thresholds a transport offers. The two that
// are not on the urgency ladder are the point: a user who asked for critical-only
// still hears that they lost money, and is not woken to be told something ended.
func TestTierRanking(t *testing.T) {
	t.Parallel()

	cases := []struct {
		tier      store.Tier
		min       config.MinTier
		delivered bool
		why       string
	}{
		{store.TierLoss, config.MinTierCritical, true, "a loss must reach a critical-only transport"},
		{store.TierCritical, config.MinTierCritical, true, ""},
		{store.TierResolved, config.MinTierCritical, false, "nobody is paged to be told it is over"},
		{store.TierWarning, config.MinTierCritical, false, ""},
		{store.TierInfo, config.MinTierCritical, false, ""},

		{store.TierWarning, config.MinTierWarning, true, ""},
		{store.TierResolved, config.MinTierWarning, false, "a resolution ranks as info"},
		{store.TierLoss, config.MinTierWarning, true, ""},

		{store.TierInfo, config.MinTierInfo, true, ""},
		{store.TierResolved, config.MinTierInfo, true, ""},

		// An unset threshold means the user never chose one, which is not the same
		// as asking for critical-only.
		{store.TierInfo, config.MinTier(""), true, "an unconfigured transport still rings"},
	}

	for _, tc := range cases {
		got := Deliverable(tc.tier, tc.min)
		if got != tc.delivered {
			t.Errorf("Deliverable(%q, %q) = %v, want %v — %s",
				tc.tier, tc.min, got, tc.delivered, tc.why)
		}
	}

	// An alert nobody recognised is delivered rather than dropped: failing towards
	// silence is the wrong direction for an alarm.
	if !Deliverable(store.Tier("something_new"), config.MinTierCritical) {
		t.Error("an unrecognised tier was silently filtered out")
	}
}

func TestUrgent(t *testing.T) {
	t.Parallel()

	for _, tier := range []store.Tier{store.TierCritical, store.TierLoss} {
		if !Urgent(tier) {
			t.Errorf("%q is not repeated until acknowledged", tier)
		}
	}
	for _, tier := range []store.Tier{store.TierInfo, store.TierWarning, store.TierResolved} {
		if Urgent(tier) {
			t.Errorf("%q would be repeated until acknowledged, which trains people to ignore alerts", tier)
		}
	}
}

// Every decision about what the user is told, and how urgently, is in one table.
func TestMapEventToAlert(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		event    bus.Event
		wantTier store.Tier
		wantKind string
		wantKey  string
	}{
		{
			name:     "watching starts",
			event:    bus.SplitStateChanged{Old: "UNARMED", New: "ARMED"},
			wantTier: store.TierInfo,
			wantKind: KindWatchingStarted,
			wantKey:  KindWatchingStarted,
		},
		{
			name:     "the chains separate",
			event:    bus.SplitStateChanged{Old: "ARMED", New: "SPLIT"},
			wantTier: store.TierWarning,
			wantKind: KindSplitDetected,
			wantKey:  KindSplitDetected,
		},
		{
			name:     "the split may be ending",
			event:    bus.SplitStateChanged{Old: "SPLIT", New: "RESOLVING"},
			wantTier: store.TierInfo,
			wantKind: KindSplitResolving,
			wantKey:  KindSplitResolving,
		},
		{
			name:     "an operator records the outcome",
			event:    bus.SplitStateChanged{Old: "RESOLVING", New: "RESOLVED_SF_WON"},
			wantTier: store.TierResolved,
			wantKind: KindSplitResolved,
			wantKey:  KindSplitResolved,
		},
		{
			name:     "the other outcome",
			event:    bus.SplitStateChanged{Old: "RESOLVING", New: "RESOLVED_SQ_WON"},
			wantTier: store.TierResolved,
			wantKind: KindSplitResolved,
			wantKey:  KindSplitResolved,
		},
		{
			// Watching stopping is the failure this project cares most about: the
			// daemon looks alive while doing nothing.
			name:     "watching stops",
			event:    bus.SplitStateChanged{Old: "ARMED", New: "UNARMED"},
			wantTier: store.TierWarning,
			wantKind: KindWatchingStopped,
			wantKey:  KindWatchingStopped,
		},
		{
			name:     "a view degrades",
			event:    bus.ViewHealthChanged{View: "sq", Old: "OK", New: "DOWN"},
			wantTier: store.TierWarning,
			wantKind: KindViewDegraded,
			wantKey:  KindViewDegraded + ":sq",
		},
		{
			name:     "a view is still syncing",
			event:    bus.ViewHealthChanged{View: "sq", Old: "OK", New: "SYNCING"},
			wantTier: store.TierWarning,
			wantKind: KindViewDegraded,
			wantKey:  KindViewDegraded + ":sq",
		},
		{
			name:     "we may not be seeing the whole picture",
			event:    bus.ViewHealthChanged{View: "sq", Old: "OK", New: "ECLIPSE_SUSPECT"},
			wantTier: store.TierWarning,
			wantKind: KindViewDegraded,
			wantKey:  KindViewDegraded + ":sq",
		},
		{
			// Urgent although nothing is under attack: watching is paused, so the
			// daemon is reporting calm about a chain nobody needs watched, and it
			// stays broken until someone changes the configuration.
			name:     "a view is on the wrong chain",
			event:    bus.ViewHealthChanged{View: "sq", Old: "OK", New: "WRONG_BRANCH"},
			wantTier: store.TierCritical,
			wantKind: KindViewWrongBranch,
			wantKey:  KindViewWrongBranch + ":sq",
		},
		{
			name:     "a view recovers",
			event:    bus.ViewHealthChanged{View: "sf", Old: "DOWN", New: "OK"},
			wantTier: store.TierResolved,
			wantKind: KindViewRecovered,
			wantKey:  KindViewRecovered + ":sf",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := MapEventToAlert(tc.event)
			if !ok {
				t.Fatal("no alert was raised for an event that warrants one")
			}
			if got.Tier != tc.wantTier {
				t.Errorf("tier = %q, want %q", got.Tier, tc.wantTier)
			}
			if got.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if got.DedupKey != tc.wantKey {
				t.Errorf("dedup key = %q, want %q", got.DedupKey, tc.wantKey)
			}
			if got.Message == "" {
				t.Error("the message is empty, so a user would be shown a bare tier name")
			}
			if !got.Tier.Valid() {
				t.Errorf("tier %q is not one the store will accept", got.Tier)
			}
		})
	}
}

// A degraded view and a recovered view must not share a dedup key. The store
// keeps an alert's original message, so one key per view would leave a row whose
// text says "having trouble seeing the other chain" while reporting a recovery.
func TestDegradedAndRecoveredAreSeparateAlerts(t *testing.T) {
	t.Parallel()

	down, _ := MapEventToAlert(bus.ViewHealthChanged{View: "sq", Old: "OK", New: "DOWN"})
	up, _ := MapEventToAlert(bus.ViewHealthChanged{View: "sq", Old: "DOWN", New: "OK"})

	if down.DedupKey == up.DedupKey {
		t.Errorf("both use dedup key %q, so a recovery would inherit the outage's text",
			down.DedupKey)
	}
}

// One view's problems must not overwrite the other's.
func TestTheTwoViewsGetSeparateAlerts(t *testing.T) {
	t.Parallel()

	sf, _ := MapEventToAlert(bus.ViewHealthChanged{View: "sf", Old: "OK", New: "DOWN"})
	sq, _ := MapEventToAlert(bus.ViewHealthChanged{View: "sq", Old: "OK", New: "DOWN"})

	if sf.DedupKey == sq.DedupKey {
		t.Error("both views share a dedup key, so only one outage would ever be reported")
	}
	if sf.Subject == sq.Subject {
		t.Errorf("both views are described as %q", sf.Subject)
	}
}

func TestEventsThatWarrantNoAlert(t *testing.T) {
	t.Parallel()

	cases := map[string]bus.Event{
		// Block-by-block telemetry. Alerting on it would bury everything else.
		"a branch extends": bus.SplitBranchExtended{Branch: "sq"},
		// Consuming this would be a loop: the alerter publishes it.
		"an alert is raised": bus.AlertRaised{AlertID: 1, Tier: "critical"},
		// Re-entering ARMED from anywhere but UNARMED would be a state machine
		// that had gone backwards, not news.
		"armed from split": bus.SplitStateChanged{Old: "SPLIT", New: "ARMED"},
		"an unknown state": bus.SplitStateChanged{Old: "ARMED", New: "SOMETHING_NEW"},
		"an unknown view state": bus.ViewHealthChanged{
			View: "sq", Old: "OK", New: "SOMETHING_NEW",
		},
	}

	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, ok := MapEventToAlert(e); ok {
				t.Errorf("raised %q, want no alert", got.Kind)
			}
		})
	}
}

// Every health state the chain views can report has to map to something. A state
// that fell through would be a condition nobody is ever told about.
func TestEveryHealthStateMapsToAnAlert(t *testing.T) {
	t.Parallel()

	states := []chainview.HealthState{
		chainview.HealthOK, chainview.HealthSyncing, chainview.HealthDegraded,
		chainview.HealthEclipseSuspect, chainview.HealthWrongBranch, chainview.HealthDown,
	}
	for _, st := range states {
		if !st.Valid() {
			t.Fatalf("%q is not a state the views report — this list has drifted", st)
		}
		if _, ok := MapEventToAlert(bus.ViewHealthChanged{View: "sq", New: string(st)}); !ok {
			t.Errorf("health state %q produces no alert", st)
		}
	}
}

// The whole point of the default. The operator of a third-party notification
// service is an actor in the threat model: an alert that names the subject tells
// them this user is under attack and roughly when, which is the attacker's ideal
// input.
func TestContentFreePayloadCarriesNothingSpecific(t *testing.T) {
	t.Parallel()

	a := store.Alert{
		Tier:    store.TierCritical,
		Kind:    KindViewWrongBranch,
		Subject: "the other chain",
		Message: "Setup problem: Forktower paused watching, 40 blocks remain on channel abc.",
	}

	p := PayloadFor(a, false)

	if p.Subject != "" {
		t.Errorf("subject %q was sent to a transport configured for no detail", p.Subject)
	}
	if strings.Contains(p.Message, "40 blocks") || strings.Contains(p.Message, "abc") {
		t.Errorf("the detailed message leaked into a content-free payload: %q", p.Message)
	}
	if p.Message != ContentFreeMessage {
		t.Errorf("message = %q, want the standard instruction", p.Message)
	}
	// Tier and kind stay: a notification with no severity or category cannot be
	// triaged, and one with no instruction cannot be acted on.
	if p.Tier != string(store.TierCritical) || p.Kind != KindViewWrongBranch {
		t.Errorf("tier or kind was stripped: %+v", p)
	}
	if p.Version != PayloadVersion {
		t.Errorf("version = %q, want %q", p.Version, PayloadVersion)
	}
}

func TestDetailedPayloadCarriesTheRealMessage(t *testing.T) {
	t.Parallel()

	a := store.Alert{
		Tier:    store.TierWarning,
		Kind:    KindViewDegraded,
		Subject: "the other chain",
		Message: "Forktower is having trouble seeing the other chain.",
	}

	p := PayloadFor(a, true)
	if p.Subject != a.Subject || p.Message != a.Message {
		t.Errorf("got %+v, want the alert's own subject and message", p)
	}
}

// A transport type the user configured but this build cannot deliver to is
// refused loudly. Skipping it silently would leave someone believing they are
// covered by a channel that will never fire.
func TestRoutesFromConfig(t *testing.T) {
	t.Parallel()

	routes, err := RoutesFromConfig([]config.TransportConfig{{
		Name: "my-server", Type: config.TransportWebhook,
		URL: "https://hooks.example.com/x", MinTier: config.MinTierWarning,
	}}, 0)
	if err != nil {
		t.Fatalf("a valid webhook was refused: %v", err)
	}
	if len(routes) != 1 || routes[0].Transport.Name() != "my-server" {
		t.Fatalf("got %d routes, want one named my-server", len(routes))
	}
	if routes[0].MinTier != config.MinTierWarning {
		t.Errorf("min tier = %q, want warning", routes[0].MinTier)
	}
	// Third-party by default: the operator of a notification service is an actor
	// in the threat model.
	if routes[0].IncludeDetail {
		t.Error("a webhook defaulted to sending detail to a third party")
	}

	if _, err := RoutesFromConfig([]config.TransportConfig{
		{Name: "n", Type: config.TransportType("carrier-pigeon")},
	}, 0); err == nil {
		t.Error("an unknown transport type was accepted")
	}

	if _, err := RoutesFromConfig([]config.TransportConfig{
		{Name: "n", Type: config.TransportWebhook, URL: "not a url"},
	}, 0); err == nil {
		t.Error("a webhook with an unusable url was accepted")
	}
}

// include_detail is honoured when the user sets it explicitly, both ways.
func TestRoutesFromConfigHonoursAnExplicitDetailSetting(t *testing.T) {
	t.Parallel()

	on, off := true, false
	for _, tc := range []struct {
		set  *bool
		want bool
	}{{&on, true}, {&off, false}, {nil, false}} {
		routes, err := RoutesFromConfig([]config.TransportConfig{{
			Name: "n", Type: config.TransportWebhook,
			URL: "https://hooks.example.com/x", IncludeDetail: tc.set,
		}}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if routes[0].IncludeDetail != tc.want {
			t.Errorf("include_detail resolved to %v, want %v", routes[0].IncludeDetail, tc.want)
		}
	}
}

func TestNewRejectsAnUnusableAlerter(t *testing.T) {
	t.Parallel()

	if _, err := New(nil, nil, nil, Config{}, nil, nil); err == nil {
		t.Error("an alerter with no store was accepted")
	}
}

func TestNewRejectsUnusableRoutes(t *testing.T) {
	t.Parallel()

	st := openTempStore(t)
	b := bus.New(nil)
	t.Cleanup(b.Close)

	if _, err := New(st, nil, nil, Config{}, nil, nil); err == nil {
		t.Error("an alerter with no event bus was accepted")
	}
	if _, err := New(st, b, []Route{{}}, Config{}, nil, nil); err == nil {
		t.Error("a route with no transport was accepted")
	}
	if _, err := New(st, b, []Route{{Transport: newRecorder("")}}, Config{}, nil, nil); err == nil {
		t.Error("a transport with no name was accepted, but every delivery is recorded against it")
	}
}

// Everything optional has a working default, so a minimal configuration is a
// functioning one rather than a silently inert one.
func TestConfigDefaults(t *testing.T) {
	t.Parallel()

	got := Config{}.withDefaults()
	if got.CriticalRepeat != DefaultCriticalRepeat {
		t.Errorf("CriticalRepeat = %v, want %v", got.CriticalRepeat, DefaultCriticalRepeat)
	}
	if got.ScanInterval != DefaultScanInterval {
		t.Errorf("ScanInterval = %v, want %v", got.ScanInterval, DefaultScanInterval)
	}
	if got.SendTimeout != DefaultSendTimeout {
		t.Errorf("SendTimeout = %v, want %v", got.SendTimeout, DefaultSendTimeout)
	}
	// The scan has to be well inside the repeat interval, or a repeat lands up to
	// a whole scan late and the escalation is slower than it claims to be.
	if got.ScanInterval >= got.CriticalRepeat {
		t.Errorf("scanning every %v cannot deliver a repeat every %v",
			got.ScanInterval, got.CriticalRepeat)
	}
}

// A branch name from a future version must still produce readable text rather
// than an empty phrase in the middle of a sentence.
func TestViewLabelForAnUnknownBranch(t *testing.T) {
	t.Parallel()

	c, ok := MapEventToAlert(bus.ViewHealthChanged{View: "something-new", New: "DOWN"})
	if !ok {
		t.Fatal("an unknown view produced no alert at all")
	}
	if c.Subject == "" {
		t.Error("an unknown view has no description")
	}
	if strings.Contains(c.Message, "  ") || strings.HasSuffix(c.Message, " .") {
		t.Errorf("the message reads badly with an unknown view: %q", c.Message)
	}
}

// Every transport type M1 claims to support must actually build, and the rest
// must be refused rather than silently skipped.
func TestRoutesFromConfigForEveryM1Transport(t *testing.T) {
	t.Parallel()

	routes, err := RoutesFromConfig([]config.TransportConfig{
		{
			Name: "hook", Type: config.TransportWebhook,
			URL: "https://example.com/hook", MinTier: config.MinTierInfo,
		},
		{
			Name: "my-ntfy", Type: config.TransportNtfy,
			URL: "https://ntfy.example.com/forktower", Token: "tk", MinTier: config.MinTierWarning,
		},
		{
			Name: "email", Type: config.TransportSMTP,
			Host: "mail.example.com", Port: 587, From: "a@example.com", To: "b@example.com",
			MinTier: config.MinTierCritical,
		},
	}, 0)
	if err != nil {
		t.Fatalf("a valid configuration was refused: %v", err)
	}
	if len(routes) != 3 {
		t.Fatalf("got %d routes, want 3", len(routes))
	}
	for i, want := range []string{"hook", "my-ntfy", "email"} {
		if routes[i].Transport.Name() != want {
			t.Errorf("route %d is %q, want %q", i, routes[i].Transport.Name(), want)
		}
		// All three are third-party, so none may default to sending detail.
		if routes[i].IncludeDetail {
			t.Errorf("transport %q defaulted to sending detail to a third party", want)
		}
	}

	notYet := []config.TransportType{
		config.TransportTelegram, config.TransportStartOS, config.TransportUmbrel,
	}
	for _, typ := range notYet {
		if _, err := RoutesFromConfig([]config.TransportConfig{{Name: "n", Type: typ}}, 0); err == nil {
			t.Errorf("type %q was accepted but nothing would ever be delivered through it", typ)
		}
	}

	// A misconfigured transport of a supported type is refused too, at startup
	// rather than during a split.
	bad := []config.TransportConfig{
		{Name: "n", Type: config.TransportNtfy, URL: "https://ntfy.sh"},
		{Name: "n", Type: config.TransportSMTP, Host: "", Port: 587},
	}
	for _, tc := range bad {
		if _, err := RoutesFromConfig([]config.TransportConfig{tc}, 0); err == nil {
			t.Errorf("accepted a %q transport that could never deliver", tc.Type)
		}
	}
}
