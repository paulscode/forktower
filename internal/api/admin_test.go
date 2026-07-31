package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/paulscode/forktower/internal/store"
	"github.com/paulscode/forktower/internal/watcher"
)

// The button exists because the daemon cannot always know it missed something. A
// node connected after a split had begun, a database restored from a backup, a
// stretch read while a backend was lying — none of those announce themselves.
func TestAskingForARescanQueuesOne(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	got := decode[RescanResult](t, h.do(t, http.MethodPost, "/api/v1/rescan", ""))
	if got.FromHeight != 500 || got.ToHeight != 600 {
		t.Errorf("queued %d..%d", got.FromHeight, got.ToHeight)
	}
	if got.Display == "" {
		t.Error("nothing was said about what is happening")
	}
	// An empty body means "from where the chains separated", which is the answer
	// almost everyone wants and the only one most people could name.
	if h.wa.read().queuedFrom != -1 {
		t.Error("an empty request did not sweep from the separation point")
	}
}

func TestARescanCanBeGivenAStartingHeight(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	got := decode[RescanResult](t,
		h.do(t, http.MethodPost, "/api/v1/rescan", `{"from_height":961633}`))
	if got.FromHeight != 961_633 {
		t.Errorf("queued from %d", got.FromHeight)
	}
	if h.wa.read().queuedFrom != 961_633 {
		t.Errorf("the watcher was asked for %d", h.wa.read().queuedFrom)
	}
}

// A button that reports success without having done anything is how somebody
// comes to believe a check has been run.
func TestARescanWithNothingToDoIsRefusedNotCelebrated(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.wa.set(func(f *fakeWatcher) { f.refuse = true })

	resp := h.do(t, http.MethodPost, "/api/v1/rescan", "")
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("returned %d, want a refusal", resp.StatusCode)
	}
	if got := errorCode(t, resp); got != CodeWrongState {
		t.Errorf("code = %q", got)
	}
}

func TestARescanFromANonsenseHeightIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	resp := h.do(t, http.MethodPost, "/api/v1/rescan", `{"from_height":-5}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("returned %d", resp.StatusCode)
	}
	if h.wa.read().calls != 0 {
		t.Error("a nonsense height still reached the watcher")
	}

	resp = h.do(t, http.MethodPost, "/api/v1/rescan", `not json`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unreadable body returned %d", resp.StatusCode)
	}
}

// The refusal that is the whole reason this is a separate endpoint from
// confirming a split is over. One authenticated POST, or one mis-click during a
// live incident, must not switch off the defence while it is doing something.
func TestStandingDownIsRefusedWhileAnythingIsCountingDown(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	id := addChannel(t, h, fundingA, nil)
	spendID := addSpend(t, h, id, nil)
	addDeadline(t, h, spendID, nil)

	resp := h.do(t, http.MethodPost, "/api/v1/watch/stand-down", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("returned %d, want a refusal", resp.StatusCode)
	}
	if got := errorCode(t, resp); got != CodeDeadlinesCounting {
		t.Errorf("code = %q, want %q", got, CodeDeadlinesCounting)
	}
	if !h.sd.Active() {
		t.Error("watching was stood down despite the refusal")
	}
}

func TestStandingDownAndResuming(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	down := decode[standDownResult](t, h.do(t, http.MethodPost, "/api/v1/watch/stand-down", ""))
	if down.WatchingActive {
		t.Error("standing down reported watching as still active")
	}
	if down.Display == "" {
		t.Error("nothing was said about what changed")
	}
	if h.sd.Active() {
		t.Error("the switch was not actually thrown")
	}

	// And it says so on the page, plainly, with a way back.
	item := itemByID(t, h.srv.Readiness(context.Background()), CheckWatchingActive)
	if item.OK {
		t.Error("a stood-down daemon reports watching as fine")
	}
	if item.Action == nil || item.Action.Endpoint != PathResume {
		t.Errorf("no way back was offered: %+v", item.Action)
	}

	up := decode[standDownResult](t, h.do(t, http.MethodPost, "/api/v1/watch/resume", ""))
	if !up.WatchingActive || h.sd.Active() != true {
		t.Error("resuming did not resume")
	}
	if !itemByID(t, h.srv.Readiness(context.Background()), CheckWatchingActive).OK {
		t.Error("the page still says watching is off")
	}
}

// A countdown that has finished is not a reason to keep the defence switched on
// for ever.
func TestStandingDownIsAllowedOnceNothingIsCounting(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	id := addChannel(t, h, fundingA, nil)
	spendID := addSpend(t, h, id, nil)
	addDeadline(t, h, spendID, func(d *store.Deadline) { d.State = store.DeadlineResolved })

	resp := h.do(t, http.MethodPost, "/api/v1/watch/stand-down", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("returned %d", resp.StatusCode)
	}
}

// The failure this check exists for is a quiet one: the mark only moves after a
// block commits, so a block that fails every attempt freezes it while everything
// else stays green.
func TestTheReadingCheckNoticesAFrozenScan(t *testing.T) {
	t.Parallel()

	t.Run("reading normally", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		item := itemByID(t, h.srv.Readiness(context.Background()), CheckWatcherProgressing)
		if !item.OK {
			t.Errorf("a healthy scan reported as %q", item.Label)
		}
	})

	t.Run("stuck", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		h.wa.set(func(f *fakeWatcher) {
			f.progress = watcher.Progress{
				Height: 900, Stalled: true, StalledAt: 901,
				Why: "a block could not be read after several tries",
			}
		})

		items := h.srv.Readiness(context.Background())
		item := itemByID(t, items, CheckWatcherProgressing)
		if item.OK {
			t.Error("a frozen scan reported as fine")
		}
		// A real failure, not an informational one: nothing new is being checked.
		var blocking bool
		for _, f := range blockingFailures(items) {
			if f.ID == CheckWatcherProgressing {
				blocking = true
			}
		}
		if !blocking {
			t.Error("a frozen scan was hidden from the headline")
		}
		if item.Detail == "" {
			t.Error("no explanation of what it is stuck on")
		}
	})

	t.Run("nothing read yet", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		h.wa.set(func(f *fakeWatcher) { f.progress = watcher.Progress{} })

		items := h.srv.Readiness(context.Background())
		item := itemByID(t, items, CheckWatcherProgressing)
		if item.OK {
			t.Error("having read nothing reported as fine")
		}
		// Starting up is not a fault.
		for _, f := range blockingFailures(items) {
			if f.ID == CheckWatcherProgressing {
				t.Error("a daemon that has only just started is dragging the headline down")
			}
		}
	})

	t.Run("catching up", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, nil)
		h.wa.set(func(f *fakeWatcher) {
			f.progress = watcher.Progress{Height: 900, RescanNext: 500, RescanTarget: 900}
		})

		item := itemByID(t, h.srv.Readiness(context.Background()), CheckWatcherProgressing)
		if !item.OK {
			t.Errorf("catching up reported as a fault: %q", item.Label)
		}
		if !strings.Contains(strings.ToLower(item.Label), "catching up") {
			t.Errorf("label = %q", item.Label)
		}
	})
}

// These change what the daemon does, so they are refused twice over: once for
// coming from somewhere that is not the dashboard, and once for having no
// session. The order matters less than that neither can be skipped.
func TestTheAdminEndpointsNeedAuthentication(t *testing.T) {
	t.Parallel()
	h := passwordHarness(t)

	paths := []string{PathRescan, PathStandDown, PathResume}

	// From the dashboard, but without having signed in.
	for _, path := range paths {
		resp := h.do(t, http.MethodPost, path, "")
		if got := errorCode(t, resp); got != CodeUnauthorized {
			t.Errorf("%s answered %q without a session, want %q",
				path, got, CodeUnauthorized)
		}
	}

	// And from somewhere that is not the dashboard at all, which is refused
	// before the question of a session even arises — a page on another site must
	// not be able to switch off somebody's defence.
	for _, path := range paths {
		resp := h.doWith(t, http.MethodPost, path, "", func(r *http.Request) {
			r.Header.Set("Origin", "https://somewhere-else.example")
		})
		if got := errorCode(t, resp); got != CodeBadOrigin {
			t.Errorf("%s answered %q to a foreign origin, want %q",
				path, got, CodeBadOrigin)
		}
	}
}
