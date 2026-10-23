package provider

// The frozen WorkBuddy accounts' probe (workbuddy_warmup.go): when it is
// due, what a 200 does, and what anything else does.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/plugin"
)


// wbResetsHere gives the probe routing's reset reader, as the gateway does
// at startup: the words WorkBuddy names a window's start with.
func wbResetsHere(t *testing.T, f func([]byte, time.Time) time.Duration) {
	t.Helper()
	old := wbResets
	SetWorkBuddyResets(f)
	t.Cleanup(func() { wbResets = old })
}

// wbAt is a fixed instant, so a test says the times it means.
func wbAt(min int) time.Time {
	return time.Date(2026, 10, 22, 12, min, 0, 0, time.UTC)
}

// wbProbeHere gives the probe an address, as the gateway does at startup.
func wbProbeHere(t *testing.T, url string) {
	t.Helper()
	old := wbProbeURL
	SetWorkBuddyProbeURL(url)
	t.Cleanup(func() { wbProbeURL = old })
}

// wbFrozenAt keeps one account frozen with its reset at until.
func wbFrozenAt(t *testing.T, user, model string, until time.Time) {
	t.Helper()
	ResetWorkBuddyWarmed()
	t.Cleanup(ResetWorkBuddyWarmed)
	NoteWorkBuddyFrozen("plugin:workbuddy-ai", user, model, until, until.Add(-24*time.Hour))
}

// wbGot is the entry kept for one account's one model.
func wbGot(user, model string) (wbWarm, bool) {
	w, ok := WorkBuddyWarmed()[wbKey(user, model)]
	return w, ok
}

// wbSend is a probe that answers status, and counts its calls.
func wbSend(status int, calls *int) func(context.Context, string, string, string) (int, error) {
	return func(context.Context, string, string, string) (int, error) {
		*calls++
		return status, nil
	}
}

// A freeze is kept, and a second one for the same window doesn't move it:
// a request that fails again while the account rests says nothing new.
func TestWorkBuddyFrozenKept(t *testing.T) {
	ResetWorkBuddyWarmed()
	t.Cleanup(ResetWorkBuddyWarmed)
	until := wbAt(30)
	NoteWorkBuddyFrozen("plugin:workbuddy-ai", "a@b.c", "deepseek-v4.1-flash", until, wbAt(0))
	got, _ := wbGot("a@b.c", "deepseek-v4.1-flash")
	if got.Until != until {
		t.Fatalf("until = %s, want %s", got.Until, until)
	}
	if got.Model != "deepseek-v4.1-flash" || got.Provider != "workbuddy-ai" {
		t.Fatalf("kept %+v", got)
	}
	if !got.NextAt.After(until) || got.NextAt.Sub(until) > wbWarmFirst {
		t.Fatalf("first ask at %s, want within %s after the reset %s", got.NextAt, wbWarmFirst, until)
	}
	// the same window again: the entry stands as it was
	NoteWorkBuddyFrozen("plugin:workbuddy-ai", "a@b.c", "deepseek-v4.1-flash", until, wbAt(5))
	if again, _ := wbGot("a@b.c", "deepseek-v4.1-flash"); !again.FrozenAt.Equal(wbAt(0)) {
		t.Fatalf("a second freeze of the same window rewrote it: %s", again.FrozenAt)
	}
}

// Only WorkBuddy's accounts are kept, and only with a reset.
func TestWorkBuddyFrozenOnlyWorkBuddy(t *testing.T) {
	ResetWorkBuddyWarmed()
	t.Cleanup(ResetWorkBuddyWarmed)
	NoteWorkBuddyFrozen("plugin:qoder", "a@b.c", "m", wbAt(30), wbAt(0))
	NoteWorkBuddyFrozen("codex", "a@b.c", "m", wbAt(30), wbAt(0))
	NoteWorkBuddyFrozen("plugin:workbuddy-ai", "", "m", wbAt(30), wbAt(0))
	NoteWorkBuddyFrozen("plugin:workbuddy-ai", "a@b.c", "m", time.Time{}, wbAt(0))
	if got := WorkBuddyWarmed(); len(got) != 0 {
		t.Fatalf("kept %d, want none: %+v", len(got), got)
	}
	// WorkBuddy (the China build) is kept as WorkBuddy AI is
	NoteWorkBuddyFrozen("plugin:workbuddy", "c@d.e", "m", wbAt(30), wbAt(0))
	if _, ok := wbGot("c@d.e", "m"); !ok {
		t.Fatal("the China build's account wasn't kept")
	}
}

// Nothing is asked before the reset, and one is asked once it has come.
func TestWorkBuddyWarmDue(t *testing.T) {
	until := wbAt(30)
	wbFrozenAt(t, "a@b.c", "m", until)
	calls := 0
	// a minute before the reset: not yet
	wbWarmNow(context.Background(), until.Add(-time.Minute), wbSend(200, &calls))
	if calls != 0 {
		t.Fatalf("asked %d times before the reset", calls)
	}
	// past its wait: asked
	wbWarmNow(context.Background(), until.Add(wbWarmFirst+time.Second), wbSend(200, &calls))
	if calls != 1 {
		t.Fatalf("asked %d times past the wait, want 1", calls)
	}
}

// A 200 counts the account back in and drops it: nothing is asked again.
func TestWorkBuddyWarmAnswered200(t *testing.T) {
	until := wbAt(30)
	wbFrozenAt(t, "a@b.c", "m", until)
	calls := 0
	now := until.Add(wbWarmFirst + time.Second)
	wbWarmNow(context.Background(), now, wbSend(http.StatusOK, &calls))
	got, kept := wbGot("a@b.c", "m")
	if !kept {
		t.Fatal("an account that answered 200 was dropped instead of watched")
	}
	if !got.Healthy || got.Attempts != 0 {
		t.Fatalf("kept %+v, want healthy with no attempts", got)
	}
	// asked again a watch period later, not before
	wbWarmNow(context.Background(), now.Add(wbWatchEvery/2), wbSend(http.StatusOK, &calls))
	if calls != 1 {
		t.Fatalf("asked %d times within the watch period, want 1", calls)
	}
	wbWarmNow(context.Background(), now.Add(wbWatchEvery+wbWarmFirst+time.Second), wbSend(http.StatusOK, &calls))
	if calls != 2 {
		t.Fatalf("asked %d times past the watch period, want 2", calls)
	}
}

// An account watched and answering is never given up on, however long it
// keeps answering: the budget is for one that stays refused.
func TestWorkBuddyWarmHealthyNeverGivesUp(t *testing.T) {
	until := wbAt(30)
	wbFrozenAt(t, "a@b.c", "m", until)
	calls := 0
	now := until.Add(wbWarmFirst + time.Second)
	wbWarmNow(context.Background(), now, wbSend(http.StatusOK, &calls))
	// long past the budget
	late := now.Add(wbWarmBudget * 4)
	wbWarmNow(context.Background(), late, wbSend(http.StatusOK, &calls))
	if got, _ := wbGot("a@b.c", "m"); got.GaveUp {
		t.Fatal("a healthy account was given up on")
	}
	if calls != 2 {
		t.Fatalf("asked %d times, want 2", calls)
	}
}

// A watched account that answers and then is refused goes back to being
// frozen, and is asked on the retry schedule again.
func TestWorkBuddyWarmHealthyThenFrozen(t *testing.T) {
	until := wbAt(30)
	wbFrozenAt(t, "a@b.c", "m", until)
	calls := 0
	now := until.Add(wbWarmFirst + time.Second)
	wbWarmNow(context.Background(), now, wbSend(http.StatusOK, &calls))
	again := now.Add(wbWatchEvery + wbWarmFirst + time.Second)
	wbWarmNow(context.Background(), again, wbSend(http.StatusTooManyRequests, &calls))
	got, _ := wbGot("a@b.c", "m")
	if got.Healthy {
		t.Fatal("a refused account is still marked healthy")
	}
	if got.Attempts != 1 || got.Status != http.StatusTooManyRequests {
		t.Fatalf("kept %+v, want one attempt and 429", got)
	}
	wait := got.NextAt.Sub(again)
	if wait < wbWarmWaitMin || wait > wbWarmWaitMax {
		t.Fatalf("waited %s, want %s..%s", wait, wbWarmWaitMin, wbWarmWaitMax)
	}
}

// Anything but a 200 waits a random while within the bounds, and asks
// again once it is over.
func TestWorkBuddyWarmRetries(t *testing.T) {
	until := wbAt(30)
	wbFrozenAt(t, "a@b.c", "m", until)
	now := until.Add(wbWarmFirst + time.Second)
	for i := 0; i < 20; i++ {
		ResetWorkBuddyWarmed()
		NoteWorkBuddyFrozen("plugin:workbuddy-ai", "a@b.c", "m", until, until.Add(-24*time.Hour))
		calls := 0
		wbWarmNow(context.Background(), now, wbSend(http.StatusTooManyRequests, &calls))
		got, _ := wbGot("a@b.c", "m")
		if got.Attempts != 1 {
			t.Fatalf("attempts = %d, want 1", got.Attempts)
		}
		if got.Status != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", got.Status)
		}
		wait := got.NextAt.Sub(now)
		if wait < wbWarmWaitMin || wait > wbWarmWaitMax {
			t.Fatalf("waited %s, want %s..%s", wait, wbWarmWaitMin, wbWarmWaitMax)
		}
	}
	// a wait that hasn't passed asks nothing
	wbFrozenAt(t, "a@b.c", "m", until)
	calls := 0
	wbWarmNow(context.Background(), now, wbSend(429, &calls))
	wbWarmNow(context.Background(), now.Add(time.Minute), wbSend(429, &calls))
	if calls != 1 {
		t.Fatalf("asked %d times within the wait, want 1", calls)
	}
}

// Past the budget the account is given up on, and never asked again.
func TestWorkBuddyWarmGivesUp(t *testing.T) {
	until := wbAt(30)
	wbFrozenAt(t, "a@b.c", "m", until)
	calls := 0
	late := until.Add(wbWarmBudget + time.Minute)
	wbWarmNow(context.Background(), late, wbSend(429, &calls))
	if calls != 0 {
		t.Fatalf("asked %d times past the budget, want 0", calls)
	}
	if got, _ := wbGot("a@b.c", "m"); !got.GaveUp {
		t.Fatal("the account wasn't given up on")
	}
	wbWarmNow(context.Background(), late.Add(time.Hour), wbSend(429, &calls))
	if calls != 0 {
		t.Fatalf("asked %d times after giving up, want 0", calls)
	}
}

// The budget is measured from the reset, not from the freeze: a freeze is
// most of a day before the reset, and measuring from it would give up at
// the first ask.
func TestWorkBuddyWarmBudgetFromReset(t *testing.T) {
	until := wbAt(30)
	wbFrozenAt(t, "a@b.c", "m", until)
	if wbWarmBudget <= until.Sub(wbAt(0)) {
		t.Fatal("this test needs a reset later than the budget")
	}
	calls := 0
	wbWarmNow(context.Background(), until.Add(time.Minute), wbSend(http.StatusTooManyRequests, &calls))
	if calls != 1 {
		t.Fatalf("asked %d times at the reset, want 1: the budget was measured from the freeze", calls)
	}
	if got, _ := wbGot("a@b.c", "m"); got.GaveUp {
		t.Fatal("given up on at the reset")
	}
}

// A probe that fails outright is retried as a refusal is.
func TestWorkBuddyWarmProbeError(t *testing.T) {
	until := wbAt(30)
	wbFrozenAt(t, "a@b.c", "m", until)
	now := until.Add(wbWarmFirst + time.Second)
	send := func(context.Context, string, string, string) (int, error) {
		return 0, errWBFailed
	}
	wbWarmNow(context.Background(), now, send)
	got, _ := wbGot("a@b.c", "m")
	if got.Attempts != 1 || got.Err != errWBFailed.Error() {
		t.Fatalf("kept %+v, want one attempt and the error", got)
	}
}

// Every WorkBuddy account is watched, not only the one this magpie saw
// refused: one used up by another computer's request never came through
// failureOf here.
func TestWorkBuddyWatchSeedsEveryAccount(t *testing.T) {
	ResetWorkBuddyWarmed()
	t.Cleanup(ResetWorkBuddyWarmed)
	// what the usage page lists
	wbQuotas = func(context.Context) []SubscriptionQuota {
		return []SubscriptionQuota{
			{Provider: "workbuddy-ai", User: "a@b.c"},
			{Provider: "workbuddy-ai", User: "d@e.f"},
			{Provider: "workbuddy", User: "g@h.i"},
			{Provider: "qoder", User: "not-workbuddy@x.y"},
		}
	}
	t.Cleanup(func() { wbQuotas = Quotas })
	wbWatch(context.Background(), wbAt(0))
	got := WorkBuddyWarmed()
	// one entry per account's model: the plugin's models are unknown here,
	// so the free one stands in for each
	if len(got) != 3 {
		t.Fatalf("watched %d, want 3: %+v", len(got), got)
	}
	for _, user := range []string{"a@b.c", "d@e.f", "g@h.i"} {
		if _, ok := wbGot(user, wbProbeModel); !ok {
			t.Fatalf("%s isn't watched", user)
		}
	}
	if _, ok := wbGot("not-workbuddy@x.y", wbProbeModel); ok {
		t.Fatal("another vendor's account was watched")
	}
	// a second look doesn't re-seed or move what is kept
	wbWatch(context.Background(), wbAt(20))
	first, _ := wbGot("a@b.c", wbProbeModel)
	if !first.NextAt.Equal(got[wbKey("a@b.c", wbProbeModel)].NextAt) {
		t.Fatal("a second look moved an account's first ask")
	}
}

// One account is out on one model and answers on another: each is watched
// apart, so the model that answers is counted back in and the one that was
// refused stays out.
func TestWorkBuddyWarmPerModel(t *testing.T) {
	ResetWorkBuddyWarmed()
	t.Cleanup(ResetWorkBuddyWarmed)
	until := wbAt(30)
	NoteWorkBuddyFrozen("plugin:workbuddy-ai", "a@b.c", "fine", until, wbAt(0))
	now := until.Add(wbWarmFirst + time.Second)
	calls := 0
	wbWarmNow(context.Background(), now, wbSend(http.StatusOK, &calls))
	if got, _ := wbGot("a@b.c", "fine"); !got.Healthy {
		t.Fatal("the model that answered isn't counted back in")
	}
	if calls != 1 {
		t.Fatalf("asked %d times, want 1", calls)
	}
	// a second model refused later is watched on its own
	NoteWorkBuddyFrozen("plugin:workbuddy-ai", "a@b.c", "out", until, now)
	got := WorkBuddyWarmed()
	if len(got) != 2 {
		t.Fatalf("kept %d entries, want 2: %+v", len(got), got)
	}
	if w, _ := wbGot("a@b.c", "out"); w.Healthy {
		t.Fatal("the refused model was marked healthy")
	}
}

// A watched account with no model known is asked with WorkBuddy's free one.
func TestWorkBuddyWatchProbesFreeModel(t *testing.T) {
	ResetWorkBuddyWarmed()
	t.Cleanup(ResetWorkBuddyWarmed)
	wbQuotas = func(context.Context) []SubscriptionQuota {
		return []SubscriptionQuota{{Provider: "workbuddy-ai", User: "a@b.c"}}
	}
	t.Cleanup(func() { wbQuotas = Quotas })
	now := wbAt(0)
	wbWatch(context.Background(), now)
	var asked string
	send := func(_ context.Context, _, _, model string) (int, error) {
		asked = model
		return http.StatusOK, nil
	}
	wbWarmNow(context.Background(), now.Add(wbWatchSpread+time.Second), send)
	if asked != wbProbeModel {
		t.Fatalf("asked %q, want %q", asked, wbProbeModel)
	}
}

// Only the watched models are asked, and only those the plugin serves: a
// model it doesn't list would be asked and refused, which says nothing
// about the account.
func TestWorkBuddyModelsWatchedOnly(t *testing.T) {
	pp := plugin.Provider{
		ID: "workbuddy-ai",
		Models: []plugin.Model{
			{ID: "deepseek-v4.1-flash"},
			{ID: "hy3", Free: true},
		},
	}
	wbPlugins = func(string) (plugin.Provider, bool) { return pp, true }
	t.Cleanup(func() { wbPlugins = func(id string) (plugin.Provider, bool) { return PluginOf(PluginID(id)) } })
	got := wbModels("workbuddy-ai")
	if len(got) != 1 || got[0] != wbProbeModel {
		t.Fatalf("wbModels = %v, want only %q", got, wbProbeModel)
	}
	// a watched model the plugin doesn't serve is left out
	wbWatchModels = []string{"deepseek-v4.1-flash", "not-served"}
	t.Cleanup(func() { wbWatchModels = []string{wbProbeModel} })
	if got := wbModels("workbuddy-ai"); len(got) != 1 || got[0] != wbProbeModel {
		t.Fatalf("wbModels = %v, want the one served model", got)
	}
}

// A freeze for a model doesn't stand for the account's other models: the
// one that was refused is watched, the others are left as they were.
func TestWorkBuddyFrozenPerModel(t *testing.T) {
	ResetWorkBuddyWarmed()
	t.Cleanup(ResetWorkBuddyWarmed)
	until := wbAt(30)
	NoteWorkBuddyFrozen("plugin:workbuddy-ai", "a@b.c", "out", until, wbAt(0))
	NoteWorkBuddyFrozen("plugin:workbuddy-ai", "a@b.c", "fine", until, wbAt(0))
	got := WorkBuddyWarmed()
	if len(got) != 2 {
		t.Fatalf("kept %d entries, want 2 (one per model): %+v", len(got), got)
	}
	if w, ok := wbGot("a@b.c", "out"); !ok || !w.Until.Equal(until) {
		t.Fatalf("the refused model isn't kept: %+v", w)
	}
}

// A 200 is the stream's head: WorkBuddy opens the stream and puts the
// refusal inside it, so the body is what decides.
func TestWorkBuddyStreamRefused(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		refused    bool
	}{
		{
			name:    "out of quota, in the vendor's words",
			body:    "data: {\"choices\":[{\"delta\":{\"content\":\"\"}}]}\n\ndata: {\"error\":{\"message\":\"usage exceeds frequency limit, your usage will reset at 2026-10-23 19:38:30 UTC+8\"}}\n\n",
			refused: true,
		},
		{
			name:    "out of quota, as its own frame",
			body:    "data: {\"error\":{\"code\":11101,\"message\":\"the account is out\"}}\n\n",
			refused: true,
		},
		{
			name:    "a reply",
			body:    "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n",
			refused: false,
		},
		{
			name:    "nothing at all",
			body:    "",
			refused: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			why := wbStreamRefused([]byte(tc.body))
			if (why != "") != tc.refused {
				t.Fatalf("wbStreamRefused = %q, want refused=%v", why, tc.refused)
			}
		})
	}
}

// A probe with nowhere to go is not sent: the gateway sets its address at
// startup, and before that there is nothing to ask through.
func TestWorkBuddyProbeNeedsAnAddress(t *testing.T) {
	ResetWorkBuddyWarmed()
	t.Cleanup(ResetWorkBuddyWarmed)
	wbProbeHere(t, "")
	if _, err := wbProbe(context.Background(), "workbuddy-ai", "a@b.c", wbProbeModel); err == nil {
		t.Fatal("a probe with no address was sent")
	}
}

// A probe refused with a reset later than the one kept moves to it: the
// window the account was frozen in has been replaced by another, and asking
// at the old one would ask a day past the time WorkBuddy named.
func TestWorkBuddyProbeRefusalMovesToItsReset(t *testing.T) {
	ResetWorkBuddyWarmed()
	t.Cleanup(ResetWorkBuddyWarmed)
	until := wbAt(30)
	wbFrozenAt(t, "a@b.c", "m", until)
	now := until.Add(wbWarmFirst + time.Second)
	later := 20 * time.Hour
	words := "usage will reset at 2026-10-23 08:00:00 UTC+8"
	wbResetsHere(t, func(body []byte, at time.Time) time.Duration {
		if string(body) == words {
			return later
		}
		return 0
	})
	calls := 0
	send := func(context.Context, string, string, string) (int, error) {
		calls++
		return http.StatusTooManyRequests, errors.New(words)
	}
	wbWarmNow(context.Background(), now, send)
	got, _ := wbGot("a@b.c", "m")
	if want := now.Add(later); !got.Until.Equal(want) {
		t.Fatalf("until = %s, want %s", got.Until, want)
	}
	if got.NextAt.Before(got.Until) {
		t.Fatalf("next ask at %s, before the reset %s", got.NextAt, got.Until)
	}
	if calls != 1 {
		t.Fatalf("asked %d times, want 1", calls)
	}
}

// A refusal that names no reset keeps the one it had, so an account that
// says nothing is not asked again a day early.
func TestWorkBuddyProbeRefusalWithoutAResetKeepsIt(t *testing.T) {
	ResetWorkBuddyWarmed()
	t.Cleanup(ResetWorkBuddyWarmed)
	until := wbAt(30)
	wbFrozenAt(t, "a@b.c", "m", until)
	now := until.Add(wbWarmFirst + time.Second)
	wbResetsHere(t, func([]byte, time.Time) time.Duration { return 0 })
	calls := 0
	wbWarmNow(context.Background(), now, wbSend(http.StatusTooManyRequests, &calls))
	got, _ := wbGot("a@b.c", "m")
	if !got.Until.Equal(until) {
		t.Fatalf("until = %s, want the one kept, %s", got.Until, until)
	}
	wait := got.NextAt.Sub(now)
	if wait < wbWarmWaitMin || wait > wbWarmWaitMax {
		t.Fatalf("waited %s, want %s..%s", wait, wbWarmWaitMin, wbWarmWaitMax)
	}
}

type wbErr struct{}

func (wbErr) Error() string { return "no network" }

var errWBFailed = wbErr{}

// The state outlives the process: a kept freeze is read back.
func TestWorkBuddyWarmKeptOnDisk(t *testing.T) {
	ResetWorkBuddyWarmed()
	t.Cleanup(ResetWorkBuddyWarmed)
	// the directory the file lives in, as a run of magpie's has it
	if err := os.MkdirAll(filepath.Dir(wbWarmPath()), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	until := wbAt(30)
	NoteWorkBuddyFrozen("plugin:workbuddy-ai", "a@b.c", "m", until, wbAt(0))
	// what a fresh process reads
	disk := readWBWarmState(wbWarmPath())
	got, ok := disk[wbKey("a@b.c", "m")]
	if !ok {
		t.Fatal("the freeze wasn't written to disk")
	}
	if !got.Until.Equal(until) || got.Model != "m" {
		t.Fatalf("kept %+v", got)
	}
}
