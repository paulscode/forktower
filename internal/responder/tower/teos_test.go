package tower

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"time"

	"github.com/paulscode/forktower/internal/bus"
	"github.com/paulscode/forktower/internal/store"
)

const teosKey = "02abe7b6a178594b60ce4f28ddd0f69830c83d949c4330e0288be966d91ab36b7b"

func serveTeos(t *testing.T, routes map[string]func(http.ResponseWriter, *http.Request)) *Teos {
	t.Helper()
	mux := http.NewServeMux()
	for path, handler := range routes {
		mux.HandleFunc(path, handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	tw, err := NewTeos(TeosOptions{APIURL: srv.URL, Pubkey: teosKey})
	if err != nil {
		t.Fatal(err)
	}
	return tw
}

// The only thing a teos tower's public API can be asked without credentials.
func TestATeosTowerIsProvedAliveByItsPing(t *testing.T) {
	t.Parallel()
	tw := serveTeos(t, map[string]func(http.ResponseWriter, *http.Request){
		"/ping": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
	})

	id, err := tw.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.Pubkey != teosKey {
		t.Errorf("pubkey = %q, want the configured one", id.Pubkey)
	}
	if len(id.URIs) != 1 || !strings.Contains(id.URIs[0], "@") {
		t.Errorf("no address a user could paste: %+v", id.URIs)
	}
}

// A tower that is not answering must not be reported as fine just because we
// happen to know its pubkey from configuration.
func TestATeosTowerThatDoesNotAnswerIsAnError(t *testing.T) {
	t.Parallel()
	tw := serveTeos(t, map[string]func(http.ResponseWriter, *http.Request){
		"/ping": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		},
	})

	if _, err := tw.Identity(context.Background()); err == nil {
		t.Error("a tower answering 503 was reported as present")
	}
	if _, err := tw.Chain(context.Background()); err == nil {
		t.Error("a tower answering 503 reported a chain state")
	}
}

// Without a configured identity there is nothing to paste, and inventing one
// would be worse than an empty box.
func TestATeosTowerWithNoConfiguredIdentityOffersNoAddress(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	tw, err := NewTeos(TeosOptions{APIURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	id, err := tw.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(id.URIs) != 0 {
		t.Errorf("an address was invented from nothing: %+v", id.URIs)
	}
}

func TestATeosReaderNeedsAnAddress(t *testing.T) {
	t.Parallel()
	if _, err := NewTeos(TeosOptions{Pubkey: teosKey}); err == nil {
		t.Error("a teos reader with nowhere to read was built")
	}
}

// --- The Core Lightning side ---

type fakeCLN struct {
	towers []TeosTower
	err    error
}

func (f *fakeCLN) Towers(context.Context) ([]TeosTower, error) { return f.towers, f.err }

func teosMonitor(t *testing.T, client CLNTowerReader, tweak ...func(*TeosMonitorOptions)) *TeosMonitor {
	t.Helper()
	o := TeosMonitorOptions{Client: client, TowerID: 3, TowerPubkey: teosKey}
	for _, fn := range tweak {
		fn(&o)
	}
	m, err := NewTeosMonitor(o)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func healthyTeos() TeosTower {
	return TeosTower{
		ID: teosKey, NetAddr: "abcdef.onion:9814",
		Status: store.TowerReachable, AvailableSlots: 9_000,
		SubscriptionExpiry: 950_000,
	}
}

func teosConcerns(p TeosPass, kind ConcernKind) []Concern {
	var out []Concern
	for _, c := range p.Concerns {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

// **The sharpest edge on this arm, and one with no LND equivalent.** A
// subscription lapses at a block height, and a split can outlast one registered
// just before it.
func TestASubscriptionRunningOutIsSaidWhileThereIsTimeToActOnIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		expiry  int32
		tip     int32
		want    int
		mustSay string
	}{
		{"plenty of time", 950_000, 900_000, 0, ""},
		{"getting close", 900_500, 900_000, 1, "runs out in about"},
		{"exactly at the warning line", 901_000, 900_000, 1, "runs out in about"},
		{"already gone", 899_000, 900_000, 1, "has expired"},
	} {
		tower := healthyTeos()
		tower.SubscriptionExpiry = tc.expiry
		m := teosMonitor(t, &fakeCLN{towers: []TeosTower{tower}})

		pass, err := m.Check(context.Background(), tc.tip)
		if err != nil {
			t.Fatal(err)
		}
		got := teosConcerns(pass, ConcernSubscriptionExpiring)
		if len(got) != tc.want {
			t.Errorf("%s: got %d concerns, want %d", tc.name, len(got), tc.want)
			continue
		}
		if tc.want > 0 && !strings.Contains(got[0].Message, tc.mustSay) {
			t.Errorf("%s: message %q does not say %q", tc.name, got[0].Message, tc.mustSay)
		}
	}
}

// The warning has to arrive far enough ahead to be acted on unhurriedly, which
// on a slow minority branch means a lot of wall-clock time per block.
func TestTheSubscriptionWarningLeavesRoomToReRegister(t *testing.T) {
	t.Parallel()
	const aWeekOfOrdinaryBlocks = 1008
	if SubscriptionWarningBlocks < aWeekOfOrdinaryBlocks-100 {
		t.Errorf("the warning arrives %d blocks ahead, which is under a week even at "+
			"ordinary cadence — too late to re-register unhurriedly",
			SubscriptionWarningBlocks)
	}
}

// Core Lightning revokes a state and carries on without waiting for the plugin,
// so a queue that is not draining is protection quietly not happening.
func TestUndeliveredAppointmentsAreReportedBecauseNothingElseWould(t *testing.T) {
	t.Parallel()
	tower := healthyTeos()
	tower.PendingAppointments = 4
	m := teosMonitor(t, &fakeCLN{towers: []TeosTower{tower}})

	pass, err := m.Check(context.Background(), 900_000)
	if err != nil {
		t.Fatal(err)
	}
	got := teosConcerns(pass, ConcernAppointmentsUndelivered)
	if len(got) != 1 {
		t.Fatalf("got %d concerns about undelivered backups", len(got))
	}
	if !strings.Contains(got[0].Message, "moves on without waiting") {
		t.Errorf("the message does not explain why this happens silently: %q", got[0].Message)
	}
}

// Rejected appointments point at the node, not the tower, and saying which
// decides where somebody looks.
func TestRejectedAppointmentsPointAtTheNode(t *testing.T) {
	t.Parallel()
	tower := healthyTeos()
	tower.InvalidAppointments = 2
	m := teosMonitor(t, &fakeCLN{towers: []TeosTower{tower}})

	pass, err := m.Check(context.Background(), 900_000)
	if err != nil {
		t.Fatal(err)
	}
	got := teosConcerns(pass, ConcernAppointmentsInvalid)
	if len(got) != 1 || !strings.Contains(got[0].Message, "your node rather than at the tower") {
		t.Errorf("concerns = %+v", got)
	}
}

// The one place in this system where a user can be shown evidence rather than
// an inference.
func TestMisbehaviourIsReportedAsProofAndNotAsSuspicion(t *testing.T) {
	t.Parallel()
	tower := healthyTeos()
	tower.Status = store.TowerMisbehaving
	tower.MisbehavingProof = `{"locator":"aabb","recovered_id":"03cc"}`
	m := teosMonitor(t, &fakeCLN{towers: []TeosTower{tower}})

	pass, err := m.Check(context.Background(), 900_000)
	if err != nil {
		t.Fatal(err)
	}
	got := teosConcerns(pass, ConcernTowerMisbehaving)
	if len(got) != 1 {
		t.Fatalf("got %d concerns about misbehaviour", len(got))
	}
	if !strings.Contains(got[0].Message, "not a guess") {
		t.Errorf("misbehaviour was reported as suspicion: %q", got[0].Message)
	}
	if !strings.Contains(got[0].Message, "register with another") {
		t.Errorf("the message does not say what to do: %q", got[0].Message)
	}
	if pass.Health.Status != store.TowerMisbehaving {
		t.Errorf("health = %q", pass.Health.Status)
	}

	// Without the proof it is still reported, and says the evidence is missing
	// rather than implying it was seen.
	tower.MisbehavingProof = ""
	m = teosMonitor(t, &fakeCLN{towers: []TeosTower{tower}})
	pass, err = m.Check(context.Background(), 900_000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(teosConcerns(pass, ConcernTowerMisbehaving)[0].Message,
		"could not be fetched") {
		t.Error("a missing proof was not admitted to")
	}
}

// Running out of room is the same failure as expiring, arrived at differently.
func TestRunningOutOfRoomIsReported(t *testing.T) {
	t.Parallel()

	full := healthyTeos()
	full.AvailableSlots = 0
	m := teosMonitor(t, &fakeCLN{towers: []TeosTower{full}})
	pass, err := m.Check(context.Background(), 900_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(teosConcerns(pass, ConcernSlotsLow)) != 1 {
		t.Error("a tower with no room left said nothing")
	}

	// Nearly full, judged against what the subscription started with.
	nearly := healthyTeos()
	nearly.AvailableSlots = 400
	m = teosMonitor(t, &fakeCLN{towers: []TeosTower{nearly}},
		func(o *TeosMonitorOptions) { o.SlotsAtStart = 50_000 })
	pass, err = m.Check(context.Background(), 900_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(teosConcerns(pass, ConcernSlotsLow)) != 1 {
		t.Error("a tower at 400 of 50000 slots said nothing")
	}

	// With no idea what it started with, a fraction cannot be judged and is not
	// guessed at.
	m = teosMonitor(t, &fakeCLN{towers: []TeosTower{nearly}})
	pass, err = m.Check(context.Background(), 900_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(teosConcerns(pass, ConcernSlotsLow)) != 0 {
		t.Error("a fraction was reported against an unknown subscription size")
	}
}

// A node with no plugin is backing up to nothing at all, and that is an answer
// rather than a failure to read.
func TestANodeWithoutThePluginIsAnAnswer(t *testing.T) {
	t.Parallel()
	m := teosMonitor(t, &fakeCLN{err: ErrPluginNotLoaded})

	pass, err := m.Check(context.Background(), 900_000)
	if err != nil {
		t.Fatalf("a missing plugin was reported as a failure to read: %v", err)
	}
	if pass.PluginLoaded {
		t.Error("the plugin was reported as loaded")
	}
	got := teosConcerns(pass, ConcernPluginMissing)
	if len(got) != 1 || !strings.Contains(got[0].Message, "cannot install it for you") {
		t.Errorf("concerns = %+v", got)
	}
}

// With no configured identity, one tower is ours; several are ambiguous and
// attributing another tower's health to ours would be worse than saying so.
func TestOneTowerIsOursAndSeveralAreNotGuessedBetween(t *testing.T) {
	t.Parallel()

	other := healthyTeos()
	other.ID = "03" + strings.Repeat("cc", 32)

	single := &fakeCLN{towers: []TeosTower{other}}
	m := teosMonitor(t, single, func(o *TeosMonitorOptions) { o.TowerPubkey = "" })
	pass, err := m.Check(context.Background(), 900_000)
	if err != nil {
		t.Fatal(err)
	}
	if !pass.Found {
		t.Error("with one tower registered and no configured identity, it was not taken as ours")
	}

	several := &fakeCLN{towers: []TeosTower{other, healthyTeos()}}
	m = teosMonitor(t, several, func(o *TeosMonitorOptions) { o.TowerPubkey = "" })
	pass, err = m.Check(context.Background(), 900_000)
	if err != nil {
		t.Fatal(err)
	}
	if pass.Found {
		t.Error("with several towers and no configured identity, one was guessed at")
	}
	if len(teosConcerns(pass, ConcernNotRegistered)) != 1 {
		t.Error("the ambiguity was not reported at all")
	}
}

// A tower the node has never heard of is the ordinary state before registering.
func TestATeosTowerNobodyRegisteredWithIsReported(t *testing.T) {
	t.Parallel()
	other := healthyTeos()
	other.ID = "03" + strings.Repeat("cc", 32)
	m := teosMonitor(t, &fakeCLN{towers: []TeosTower{other}})

	pass, err := m.Check(context.Background(), 900_000)
	if err != nil {
		t.Fatal(err)
	}
	if pass.Found {
		t.Error("somebody else's tower was taken for ours")
	}
	if len(teosConcerns(pass, ConcernNotRegistered)) != 1 {
		t.Errorf("concerns = %+v", pass.Concerns)
	}
}

func TestAFailedPluginReadIsAnError(t *testing.T) {
	t.Parallel()
	m := teosMonitor(t, &fakeCLN{err: errors.New("connection refused")})
	if _, err := m.Check(context.Background(), 900_000); err == nil {
		t.Error("a node that could not be read produced a clean pass")
	}
	if _, err := NewTeosMonitor(TeosMonitorOptions{}); err == nil {
		t.Error("a monitor with no node to read was built")
	}
}

// The store's status vocabulary is teos's own, so this mapping should be an
// identity apart from one spelling.
func TestThePluginStatusesMapOntoTheStoresOwn(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		wire string
		want store.TowerStatus
	}{
		{"reachable", store.TowerReachable},
		{"temporary_unreachable", store.TowerTemporarilyUnreachable},
		{"temporarily_unreachable", store.TowerTemporarilyUnreachable},
		{"unreachable", store.TowerUnreachable},
		{"subscription_error", store.TowerSubscriptionError},
		{"misbehaving", store.TowerMisbehaving},
		{"Reachable", store.TowerReachable},
		{"something new", store.TowerStatusUnknown},
		{"", store.TowerStatusUnknown},
	} {
		if got := teosStatus(tc.wire); got != tc.want {
			t.Errorf("%q read as %q, want %q", tc.wire, got, tc.want)
		}
	}
}

// --- Reading the plugin over the node's REST interface ---

func serveCLN(t *testing.T, routes map[string]func(http.ResponseWriter, *http.Request)) *CLNTowers {
	t.Helper()
	mux := http.NewServeMux()
	for path, handler := range routes {
		mux.HandleFunc(path, handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	runePath := filepath.Join(t.TempDir(), "forktower.rune")
	if err := os.WriteFile(runePath, []byte("a-rune\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewCLNTowers(CLNOptions{RESTAddr: srv.URL, RunePath: runePath})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The plugin keys its towers by id, so the id arrives as an object key rather
// than as a field — which a decoder written from the struct alone would miss.
func TestTheTowerIdArrivesAsTheKeyRatherThanAField(t *testing.T) {
	t.Parallel()

	var methods, runes []string
	record := func(body string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			methods = append(methods, r.Method)
			runes = append(runes, r.Header.Get("Rune"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}
	}

	c := serveCLN(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v1/listtowers": record(`{
			"` + teosKey + `": {
				"net_addr": "abcdef.onion:9814",
				"available_slots": 8500,
				"subscription_start": 900000,
				"subscription_expiry": 952560,
				"status": "reachable",
				"pending_appointments": [],
				"invalid_appointments": []
			}
		}`),
	})

	towers, err := c.Towers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(towers) != 1 {
		t.Fatalf("got %d towers, want 1", len(towers))
	}
	if towers[0].ID != teosKey {
		t.Errorf("id = %q, want the object key", towers[0].ID)
	}
	if towers[0].AvailableSlots != 8500 || towers[0].SubscriptionExpiry != 952560 {
		t.Errorf("the subscription was misread: %+v", towers[0])
	}
	if towers[0].Status != store.TowerReachable {
		t.Errorf("status = %q", towers[0].Status)
	}
	if len(methods) != 1 || methods[0] != http.MethodPost {
		t.Errorf("calls = %v — clnrest takes a POST per method", methods)
	}
	if runes[0] == "" {
		t.Error("the credential was not sent")
	}
}

// Pending and invalid appointments arrive as lists of locators; their length is
// the number that matters.
func TestQueuedAndRejectedAppointmentsAreCounted(t *testing.T) {
	t.Parallel()
	c := serveCLN(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v1/listtowers": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"` + teosKey + `": {
				"net_addr": "x:9814", "available_slots": 10, "subscription_expiry": 1,
				"status": "reachable",
				"pending_appointments": ["aa", "bb", "cc"],
				"invalid_appointments": ["dd"]
			}}`))
		},
	})

	towers, err := c.Towers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if towers[0].PendingAppointments != 3 || towers[0].InvalidAppointments != 1 {
		t.Errorf("counts = %d pending, %d invalid",
			towers[0].PendingAppointments, towers[0].InvalidAppointments)
	}
}

// The proof is fetched only when there is one to fetch, and a failure to get it
// weakens the report rather than losing it.
func TestTheMisbehaviourProofIsFetchedWhenThereIsOne(t *testing.T) {
	t.Parallel()

	var asked int
	c := serveCLN(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v1/listtowers": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"` + teosKey + `": {
				"net_addr": "x:9814", "available_slots": 10, "subscription_expiry": 1,
				"status": "misbehaving",
				"pending_appointments": [], "invalid_appointments": []
			}}`))
		},
		"/v1/gettowerinfo": func(w http.ResponseWriter, _ *http.Request) {
			asked++
			_, _ = w.Write([]byte(`{"misbehaving_proof":{"locator":"aabb"}}`))
		},
	})

	towers, err := c.Towers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if asked != 1 {
		t.Errorf("gettowerinfo was called %d times for one misbehaving tower", asked)
	}
	if !strings.Contains(towers[0].MisbehavingProof, "aabb") {
		t.Errorf("the proof was not carried through: %q", towers[0].MisbehavingProof)
	}
}

func TestAHealthyTowerIsNotAskedForAProofItDoesNotHave(t *testing.T) {
	t.Parallel()
	var asked int
	c := serveCLN(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v1/listtowers": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"` + teosKey + `": {
				"net_addr": "x:9814", "available_slots": 10, "subscription_expiry": 1,
				"status": "reachable", "pending_appointments": [], "invalid_appointments": []
			}}`))
		},
		"/v1/gettowerinfo": func(_ http.ResponseWriter, _ *http.Request) { asked++ },
	})

	if _, err := c.Towers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if asked != 0 {
		t.Errorf("a healthy tower was asked for misbehaviour evidence %d times", asked)
	}
}

// A node without the plugin answers "unknown command", which makes an ordinary
// read a clean probe for whether it is loaded.
func TestAMissingPluginIsRecognisedFromWhatTheNodeSays(t *testing.T) {
	t.Parallel()
	c := serveCLN(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v1/listtowers": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code": -32601, "message": "Unknown command 'listtowers'"}`))
		},
	})

	_, err := c.Towers(context.Background())
	if !errors.Is(err, ErrPluginNotLoaded) {
		t.Errorf("a node without the plugin reported %v", err)
	}
}

func TestACLNReaderNeedsAnAddressAndACredential(t *testing.T) {
	t.Parallel()

	if _, err := NewCLNTowers(CLNOptions{RunePath: "/dev/null"}); err == nil {
		t.Error("a reader with no address was built")
	}
	if _, err := NewCLNTowers(CLNOptions{
		RESTAddr: "https://node:3010", RunePath: "/nowhere/forktower.rune",
	}); err == nil {
		t.Error("a reader was built with a credential that does not exist")
	}
}

// Captured from a live Core Lightning v25.09 running the plugin built from the
// vendored source, registered with a live teos on regtest.
//
// Worth having because upstream's own CI last ran against v24.11.1: nobody else
// has checked this shape against a current node, and nobody else will.
func TestTheShapeARealCoreLightningSendsIsDecoded(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join("testdata", "cln_listtowers.json"))
	if err != nil {
		t.Fatal(err)
	}

	c := serveCLN(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v1/listtowers": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(body)
		},
	})

	towers, err := c.Towers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(towers) != 1 {
		t.Fatalf("got %d towers from a real payload, want 1", len(towers))
	}
	got := towers[0]
	if !strings.HasPrefix(got.ID, "021d0a04") {
		t.Errorf("the tower id was not read from the object key: %q", got.ID)
	}
	if got.Status != store.TowerReachable {
		t.Errorf("status = %q", got.Status)
	}
	// The numbers that prove the raised subscription took effect on a real
	// tower: 150 + 52560, against a default that would have been 150 + 4320.
	if got.SubscriptionExpiry != 52710 {
		t.Errorf("subscription expiry = %d, want 52710", got.SubscriptionExpiry)
	}
	if got.AvailableSlots != 50_000 {
		t.Errorf("available slots = %d, want 50000", got.AvailableSlots)
	}
	// The plugin stores the scheme it added, so what it reports back is not what
	// a user types into registertower.
	if !strings.HasPrefix(got.NetAddr, "http://") {
		t.Errorf("net_addr = %q — the plugin reports the scheme it added", got.NetAddr)
	}
}

// `gettowerinfo` omits `misbehaving_proof` entirely when there is none, rather
// than sending null — so an absent proof must not read as a present empty one.
func TestARealTowerWithNothingToProveCarriesNoProof(t *testing.T) {
	t.Parallel()
	info, err := os.ReadFile(filepath.Join("testdata", "cln_gettowerinfo.json"))
	if err != nil {
		t.Fatal(err)
	}
	list, err := os.ReadFile(filepath.Join("testdata", "cln_listtowers.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Same tower, reported as misbehaving, so the proof is fetched.
	misbehaving := strings.Replace(string(list), `"status": "reachable"`,
		`"status": "misbehaving"`, 1)

	c := serveCLN(t, map[string]func(http.ResponseWriter, *http.Request){
		"/v1/listtowers": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(misbehaving))
		},
		"/v1/gettowerinfo": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(info)
		},
	})

	towers, err := c.Towers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if towers[0].MisbehavingProof != "" {
		t.Errorf("a tower with no proof reported one: %q", towers[0].MisbehavingProof)
	}
}

// --- The Core Lightning arm, wired into the warden ---

// **Coverage is all-or-nothing on this arm, and that is a fact about teos.**
// Core Lightning builds the penalty transaction itself and hands the tower an
// opaque blob, so a teos tower never sees a channel type: what decides whether a
// channel is protected is whether the subscription is alive, which is the same
// answer for every channel at once.
func TestATeosSubscriptionCoversEveryChannelOrNone(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		tower     TeosTower
		found     bool
		coverable bool
		mustSay   string
	}{
		{
			name: "a live subscription", tower: healthyTeos(), found: true,
			coverable: true, mustSay: "live subscription",
		},
		{
			name: "one that has run out",
			tower: func() TeosTower {
				tw := healthyTeos()
				tw.Status = store.TowerSubscriptionError
				return tw
			}(),
			found: true, coverable: false, mustSay: "run out",
		},
		{
			name: "a tower that misbehaved",
			tower: func() TeosTower {
				tw := healthyTeos()
				tw.Status = store.TowerMisbehaving
				return tw
			}(),
			found: true, coverable: false, mustSay: "does not check out",
		},
	} {
		h := newWardenHarness(t, func(o *WardenOptions) {
			o.Kind = store.TowerTeos
			o.CLNClient = &fakeCLN{towers: []TeosTower{tc.tower}}
			o.TeosPubkey = tc.tower.ID
		})
		h.addChannel(store.ChanAnchors, "aa"+strings.Repeat("0", 62))
		h.addChannel(store.ChanTaproot, "bb"+strings.Repeat("0", 62))

		h.pass()

		rows, err := h.store.ListCoverage(context.Background(), store.CoverageFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Errorf("%s: got %d coverage rows for two channels", tc.name, len(rows))
			continue
		}
		for _, r := range rows {
			if r.Coverable != tc.coverable {
				t.Errorf("%s: channel %d coverable = %v, want %v (%s)",
					tc.name, r.ChannelID, r.Coverable, tc.coverable, r.Reason)
			}
			if !strings.Contains(r.Reason, tc.mustSay) {
				t.Errorf("%s: reason %q does not say %q", tc.name, r.Reason, tc.mustSay)
			}
		}
		// **Every channel gets the same answer**, whatever its commitment type —
		// which is the point, and which would be wrong on the LND arm.
		if rows[0].Coverable != rows[1].Coverable {
			t.Errorf("%s: a taproot and an anchor channel got different answers "+
				"from a tower that never sees a channel type", tc.name)
		}
	}
}

// The concerns the Core Lightning monitor raises have to reach the warden's
// caller, or the arm is a monitor nobody listens to.
func TestTheTeosConcernsReachTheWarden(t *testing.T) {
	t.Parallel()
	expiring := healthyTeos()
	expiring.SubscriptionExpiry = 900_100
	expiring.PendingAppointments = 3

	h := newWardenHarness(t, func(o *WardenOptions) {
		o.Kind = store.TowerTeos
		o.CLNClient = &fakeCLN{towers: []TeosTower{expiring}}
		o.TeosPubkey = expiring.ID
		o.Branch = store.BranchSQ
		o.Interval = 20 * time.Millisecond
	})
	h.addChannel(store.ChanAnchors, "aa"+strings.Repeat("0", 62))
	h.start()

	// The chain the tower watches, which is what an expiry height is measured
	// against. Without it a subscription expiry means nothing, and the warden
	// only has it because it subscribed.
	h.bus.Publish(bus.SplitBranchExtended{
		Branch: string(store.BranchSQ),
		Block:  bus.BlockMetaJSON{Height: 900_000},
	})

	deadline := time.Now().Add(5 * time.Second)
	seen := map[string]bool{}
	for time.Now().Before(deadline) &&
		(!seen[string(ConcernSubscriptionExpiring)] ||
			!seen[string(ConcernAppointmentsUndelivered)]) {
		for _, e := range eventsOfKind(h.drain(), bus.KindTowerConcern) {
			if c, ok := e.(bus.TowerConcern); ok {
				seen[c.Concern] = true
			}
		}
	}
	if !seen[string(ConcernSubscriptionExpiring)] {
		t.Errorf("the subscription running out was not raised: %v", seen)
	}
	if !seen[string(ConcernAppointmentsUndelivered)] {
		t.Errorf("undelivered backups were not raised: %v", seen)
	}
}

// Without a Core Lightning node there is nothing to read, and nothing may be
// claimed about what the tower protects.
func TestATeosTowerWithNoNodeToReadClaimsNoCoverage(t *testing.T) {
	t.Parallel()
	h := newWardenHarness(t, func(o *WardenOptions) {
		o.Kind = store.TowerTeos
		o.CLNClient = nil
	})
	h.addChannel(store.ChanAnchors, "aa"+strings.Repeat("0", 62))

	h.pass()

	rows, err := h.store.ListCoverage(context.Background(), store.CoverageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("coverage was claimed with no node to read it from: %+v", rows)
	}
	// The tower itself is still watched.
	if h.tower().Status != store.TowerReachable {
		t.Error("the tower was not watched without a node to check against")
	}
}

// A node with no plugin is backing up to nothing, and every channel is
// uncovered for one reason rather than each for its own.
func TestANodeWithoutThePluginLeavesEveryChannelUncovered(t *testing.T) {
	t.Parallel()
	h := newWardenHarness(t, func(o *WardenOptions) {
		o.Kind = store.TowerTeos
		o.CLNClient = &fakeCLN{err: ErrPluginNotLoaded}
	})
	h.addChannel(store.ChanAnchors, "aa"+strings.Repeat("0", 62))

	h.pass()

	rows, err := h.store.ListCoverage(context.Background(), store.CoverageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Coverable {
		t.Fatalf("coverage = %+v", rows)
	}
	if !strings.Contains(rows[0].Reason, "not running the watchtower plugin") {
		t.Errorf("the reason does not say what is missing: %q", rows[0].Reason)
	}
}
