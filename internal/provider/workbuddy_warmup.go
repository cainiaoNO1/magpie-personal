package provider

// WorkBuddy's free models give an account 100M tokens a window, and the
// window starts over 24 hours after the first call that used it up. Until
// then WorkBuddy answers 429 and the account rests (see routing.go's
// failQuota). Nothing asks again when that 24 hours is up, so the account
// sits out until someone sends a real request and finds it working.
//
// This sends one tiny request to each frozen WorkBuddy account once its
// reset time has come, and counts the account back in only on a 200: the
// reset time WorkBuddy named is a statement of when the window starts
// over, not of when it can answer, and the two were seen 8 seconds apart
// (913553562@qq.com, 2026-10-22). Anything else is tried again after a
// random wait, so several accounts never come back in lockstep, for at
// most wbWarmBudget from the reset.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/plugin"
)

// wbWarmBudget is how long a frozen account is asked after its reset
// before this gives up on it, measured from the reset: long enough to
// cover a reset that came early, short enough to stop asking an account
// that isn't coming back.
const wbWarmBudget = 12 * time.Hour

// wbWarmWait is how long to wait before asking again, the least and the
// most: random, so the accounts a window reset together don't ask in the
// same minute.
const (
	wbWarmWaitMin = 5 * time.Minute
	wbWarmWaitMax = 15 * time.Minute
)

// wbWarmFirst is the wait past the reset time before the first ask, at
// most: WorkBuddy's own reset is what routing read, and a request in the
// same second can still be refused.
const wbWarmFirst = time.Minute

// wbWarmEvery is how often the frozen accounts are looked at.
const wbWarmEvery = time.Minute

// wbWarmProbeTimeout bounds one probe: the free model answers at once,
// and one that hangs is no answer at all.
const wbWarmProbeTimeout = 60 * time.Second

// wbWatchSpread is how long the first ask of every watched account is
// spread over, so a start doesn't ask all of them in the same minute.
const wbWatchSpread = 10 * time.Minute

// wbWatchEvery is how often every WorkBuddy account is looked for: one
// back in use is dropped, and this finds it again, so a healthy account
// is asked about this often.
const wbWatchEvery = 30 * time.Minute

// wbProbeModel is the model asked when the account was never frozen here,
// so no model is known for it: WorkBuddy's free one.
const wbProbeModel = "deepseek-v4.1-flash"

// WorkBuddyWindow is a free model's window: the 24 hours after the first
// call that used one up. A refusal that names no reset is read as one of
// these, which is what the vendor's own words say the window is; without
// it a quota refused in silence was never watched, and real requests
// found it again every quarter of an hour for a day.
const WorkBuddyWindow = 24 * time.Hour

// wbWarm is one frozen account's probe: when its window was used up, when
// WorkBuddy says it starts over, and how the asks have gone since.
type wbWarm struct {
	Provider string    `json:"provider"`
	User     string    `json:"user"`
	Model    string    `json:"model"`
	FrozenAt time.Time `json:"frozenAt"`
	// Until is when WorkBuddy says the window starts over, as routing
	// read it. The budget and the first ask are measured from it, not
	// from FrozenAt: FrozenAt is when the 429 came, and Until is most of
	// a day after it.
	Until    time.Time `json:"until"`
	NextAt   time.Time `json:"nextAt,omitzero"`
	Attempts int       `json:"attempts,omitempty"`
	Status   int       `json:"status,omitempty"`
	Err      string    `json:"err,omitempty"`
	// GaveUp is set once the budget is spent: the account is left alone
	// until a new freeze writes over it.
	GaveUp bool `json:"gaveUp,omitempty"`
	// Healthy is set once the account answered: it is watched from then
	// on, asked again every Every, rather than being frozen. Until is
	// when it answered, and the budget doesn't run against it.
	Healthy bool `json:"healthy,omitempty"`
	// Every is how often this account and model is asked while it
	// answers, the user's own setting (WorkBuddyWatch); zero is
	// wbWatchEvery. A frozen one is asked on the warm-up's own schedule
	// instead, which is what brings it back.
	Every time.Duration `json:"every,omitempty"`
}

// wbWarmState is every frozen account's probe, by lower-cased user.
type wbWarmState map[string]wbWarm

var wbWarmMu struct {
	sync.Mutex
	state wbWarmState
}

func wbWarmPath() string { return filepath.Join(filepath.Dir(Path()), "workbuddy-warmup.json") }

// readWBWarmState reads the kept state, none when it can't be read: a
// file lost only costs the accounts a fresh freeze each.
func readWBWarmState(path string) wbWarmState {
	st := wbWarmState{}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &st)
	}
	return st
}

// writeWBWarmState keeps st, with wbWarm held.
func writeWBWarmState(path string, st wbWarmState) {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	os.Rename(tmp, path)
}

// wbFrozen is a probe's provider and model as the routing trace gives
// them, to tell whether they are WorkBuddy's: "plugin:workbuddy-ai".
func wbFrozen(agent string) (id string, ok bool) {
	rest, found := strings.CutPrefix(agent, "plugin:")
	if !found {
		return "", false
	}
	switch rest {
	case "workbuddy", WorkBuddyAIID:
		return rest, true
	}
	return "", false
}

// NoteWorkBuddyFrozen records that an account of WorkBuddy's was frozen
// out of its allowance at now until until, asking model, so it is probed
// when that time comes. agent is what the account's usage is read under
// (Account.UsageAgent), "" for one that isn't a plugin's. A freeze for an
// account already kept rewrites it only when its reset moved: a request
// that fails again while the account rests says nothing new.
func NoteWorkBuddyFrozen(agent, user, model string, until, now time.Time) {
	id, ok := wbFrozen(agent)
	if !ok || user == "" || until.IsZero() {
		return
	}
	if model == "" {
		model = wbProbeModel
	}
	key := wbKey(user, model)
	wbWarmMu.Lock()
	defer wbWarmMu.Unlock()
	if wbWarmMu.state == nil {
		wbWarmMu.state = readWBWarmState(wbWarmPath())
	}
	if was, found := wbWarmMu.state[key]; found && was.Until.Equal(until) && !was.GaveUp {
		return
	}
	wbWarmMu.state[key] = wbWarm{
		Provider: id,
		User:     user,
		Model:    model,
		FrozenAt: now,
		Until:    until,
		NextAt:   until.Add(wbWarmJitter(wbWarmFirst)),
	}
	writeWBWarmState(wbWarmPath(), wbWarmMu.state)
}

// wbWarmJitter is a random wait up to d.
func wbWarmJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d)))
}

// wbWarmRetry is how long to wait after a probe that wasn't answered 200:
// random between wbWarmWaitMin and wbWarmWaitMax.
func wbWarmRetry() time.Duration {
	return wbWarmWaitMin + wbWarmJitter(wbWarmWaitMax-wbWarmWaitMin)
}

// wbQuotas is Quotas, swapped in a test so no account is really listed.
var wbQuotas = Quotas

// wbResets is routing's resetsAt, which reads when a refusal says the
// window starts over: the gateway sets it at startup
// (SetWorkBuddyResets). Without it a probe's refusal names no time, and
// the account is asked again after a short wait rather than at the reset
// it was told.
var wbResets = func(body []byte, now time.Time) time.Duration { return 0 }

// SetWorkBuddyResets lets a probe read the reset a refusal names, the way
// routing reads one for a real request.
func SetWorkBuddyResets(f func(body []byte, now time.Time) time.Duration) {
	if f != nil {
		wbResets = f
	}
}

// wbProbeURL is where the gateway listens, as a client reaches it: the
// gateway sets it at startup (SetWorkBuddyProbeURL). Empty until then, and
// a probe with nowhere to go is skipped rather than sent into nothing.
var wbProbeURL = func() string { return "" }

// SetWorkBuddyProbeURL says where the gateway listens, so a probe goes
// through it as a real request does: the plugin's own fetch answers a
// stream's head before the account is asked, and a request pinned to an
// account never leaves the gateway at all when that account rests.
func SetWorkBuddyProbeURL(u string) {
	wbProbeURL = func() string { return strings.TrimRight(u, "/") }
}

// wbPlugins is PluginOf, swapped in a test so no plugin is really loaded.
var wbPlugins = func(id string) (plugin.Provider, bool) { return PluginOf(PluginID(id)) }

// wbWatchTarget is one account's one model: WorkBuddy counts its free
// use up model by model, so an account answers on one model and is out on
// another, and each has its own window.
type wbWatchTarget struct {
	Provider string
	User     string
	Model    string
}

// wbWatchedAccounts is every WorkBuddy account the usage page lists, and
// every model of it: the ones this watches whether or not they were
// frozen. An account used up by a request of another computer's, or
// frozen before this ran, never came through failureOf here, and would
// sit out unnoticed.
func wbWatchedAccounts(ctx context.Context) []wbWatchTarget {
	var out []wbWatchTarget
	seen := map[string]bool{}
	for _, q := range wbQuotas(ctx) {
		id, ok := wbFrozen("plugin:" + q.Provider)
		if !ok || q.User == "" {
			continue
		}
		models := wbModels(id)
		if len(models) == 0 {
			models = []string{wbProbeModel}
		}
		for _, m := range models {
			key := wbKey(q.User, m)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, wbWatchTarget{Provider: id, User: q.User, Model: m})
		}
	}
	return out
}

// wbWatchModels are the models watched on every account. DeepSeek's free
// one only, for now: it is the one that runs out, and asking every free
// model of every account is more probes than the vendor answers for
// nothing. SetWorkBuddyWatch replaces it, from the Custom page.
var wbWatchModels = []string{wbProbeModel}

// WorkBuddyWatch is how the user wants the watching done: which models,
// and how often an account that answers is asked. Zero Every is
// wbWatchEvery, so a setting only says what it changes.
type WorkBuddyWatch struct {
	// Models are the models watched on every account; empty is
	// wbWatchModels.
	Models []string `json:"models,omitempty"`
	// Every is how often a healthy account and model is asked, the
	// custom mode's own interval. Zero is wbWatchEvery.
	Every time.Duration `json:"every,omitempty"`
	// Per is an account and model's own interval, over Every: the key is
	// wbKey(user, model).
	Per map[string]time.Duration `json:"per,omitempty"`
}

var wbWatchMu struct {
	sync.Mutex
	set WorkBuddyWatch
}

// WorkBuddyWatchSet is what SetWorkBuddyWatch was last given.
func WorkBuddyWatchSet() WorkBuddyWatch {
	wbWatchMu.Lock()
	defer wbWatchMu.Unlock()
	out := WorkBuddyWatch{Models: append([]string(nil), wbWatchMu.set.Models...), Every: wbWatchMu.set.Every}
	if len(wbWatchMu.set.Per) > 0 {
		out.Per = make(map[string]time.Duration, len(wbWatchMu.set.Per))
		for k, v := range wbWatchMu.set.Per {
			out.Per[k] = v
		}
	}
	return out
}

// SetWorkBuddyWatch is the watching the user set: the models watched, and
// how often a healthy account is asked. The accounts watched are seeded
// again from it, so a model added here is watched from its next tick.
func SetWorkBuddyWatch(w WorkBuddyWatch) {
	wbWatchMu.Lock()
	wbWatchMu.set = WorkBuddyWatch{Models: append([]string(nil), w.Models...), Every: w.Every}
	if len(w.Per) > 0 {
		wbWatchMu.set.Per = make(map[string]time.Duration, len(w.Per))
		for k, v := range w.Per {
			if v >= wbWatchMin {
				wbWatchMu.set.Per[k] = v
			}
		}
	}
	set := wbWatchMu.set
	wbWatchMu.Unlock()

	// a model no longer watched is dropped, so its row leaves the page
	// with it; one added is seeded on the next wbWatch. The state is read
	// first: set before the warm-up has looked at anything, its map is
	// still nil, and writing that back would empty the file.
	models := wbModelsSet(set.Models)
	wbWarmMu.Lock()
	if wbWarmMu.state == nil {
		wbWarmMu.state = readWBWarmState(wbWarmPath())
	}
	dropped := false
	for key, cur := range wbWarmMu.state {
		if !slices.Contains(models, cur.Model) {
			delete(wbWarmMu.state, key)
			dropped = true
		}
	}
	if dropped {
		writeWBWarmState(wbWarmPath(), wbWarmMu.state)
	}
	wbWarmMu.Unlock()
}

// wbWatchMin is the shortest interval a healthy account may be asked at:
// WorkBuddy counts a probe against the window like any call, and asking
// faster than this spends more than the watching is worth.
const wbWatchMin = time.Minute

// WorkBuddyWatchMin is wbWatchMin, for the page that sets an interval.
func WorkBuddyWatchMin() time.Duration { return wbWatchMin }

// WorkBuddyWatchDefault is wbWatchEvery, for the page to say what zero is.
func WorkBuddyWatchDefault() time.Duration { return wbWatchEvery }

// WorkBuddyWatchOf is a setting as the watching reads it: per is an
// account and model's own interval in seconds, keyed wbKey.
func WorkBuddyWatchOf(on bool, models []string, every time.Duration, per map[string]int) WorkBuddyWatch {
	out := WorkBuddyWatch{Every: every}
	if on {
		// on with nothing picked watches nothing: an account asked about
		// no model can't be watched at all
		out.Models = append([]string{}, models...)
	}
	if len(per) > 0 {
		out.Per = make(map[string]time.Duration, len(per))
		for k, v := range per {
			out.Per[k] = time.Duration(v) * time.Second
		}
	}
	return out
}

// wbModelsSet is models, or wbWatchModels when it names none.
func wbModelsSet(models []string) []string {
	if len(models) == 0 {
		return append([]string(nil), wbWatchModels...)
	}
	return append([]string(nil), models...)
}

// wbEveryOf is how often w's account and model is asked while it answers:
// its own interval, the setting's, or wbWatchEvery.
func wbEveryOf(w wbWarm) time.Duration {
	if w.Every > 0 {
		return w.Every
	}
	wbWatchMu.Lock()
	set := wbWatchMu.set
	wbWatchMu.Unlock()
	if per, ok := set.Per[wbKey(w.User, w.Model)]; ok && per > 0 {
		return per
	}
	if set.Every > 0 {
		return set.Every
	}
	return wbWatchEvery
}

// wbModels is the watched models p's plugin serves, in wbWatchModels'
// order: a model the plugin doesn't list is left out rather than asked
// and refused.
func wbModels(providerID string) []string {
	pp, ok := wbPlugins(providerID)
	if !ok {
		return nil
	}
	served := make(map[string]bool, len(pp.Models))
	for _, m := range pp.Models {
		served[m.ID] = true
	}
	wbWatchMu.Lock()
	wanted := wbModelsSet(wbWatchMu.set.Models)
	wbWatchMu.Unlock()
	out := make([]string, 0, len(wanted))
	for _, m := range wanted {
		if served[m] {
			out = append(out, m)
		}
	}
	return out
}

// wbWatch seeds an entry for every WorkBuddy account not kept yet, so all
// of them are watched from the start rather than from the first refusal
// this magpie itself saw. Its first ask is a little way off, spread over
// wbWatchSpread, so a fresh start doesn't ask every account at once.
func wbWatch(ctx context.Context, now time.Time) {
	seen := wbWatchedAccounts(ctx)
	wbWarmMu.Lock()
	defer wbWarmMu.Unlock()
	if wbWarmMu.state == nil {
		wbWarmMu.state = readWBWarmState(wbWarmPath())
	}
	added := false
	for _, t := range seen {
		key := wbKey(t.User, t.Model)
		if _, kept := wbWarmMu.state[key]; kept {
			continue
		}
		// not frozen, so no reset is known: ask now, and a refusal
		// writes the reset WorkBuddy names over this
		wbWarmMu.state[key] = wbWarm{
			Provider: t.Provider,
			User:     t.User,
			Model:    t.Model,
			FrozenAt: now,
			Until:    now,
			NextAt:   now.Add(wbWarmJitter(wbWatchSpread)),
		}
		added = true
	}
	if added {
		writeWBWarmState(wbWarmPath(), wbWarmMu.state)
	}
}

// wbWake is what the gateway calls to let a probe through: the account's
// rest is lifted, so the request leaves rather than being turned away
// here. Set by the gateway at startup (SetWorkBuddyWake); without it a
// probe only reports what the plugin says.
var wbWake = func(providerID, account, model string) {}

// SetWorkBuddyWake lets a probe reach the account it asks about: the
// gateway lifts the rest it made for that account and model, and puts it
// back when the probe is refused.
func SetWorkBuddyWake(f func(providerID, account, model string)) {
	if f != nil {
		wbWake = f
	}
}

// wbKey is one account's one model, as the state keeps it.
func wbKey(user, model string) string {
	return strings.ToLower(user) + "\x00" + model
}

// wbWarmDue is whether w's probe is asked at now: its reset has come, its
// wait is over, and its budget is not spent. An entry that answered 200
// has its reset at the time it answered, so it is asked again a watch
// period later, and a refusal before then moves that to the reset the
// vendor named.
func wbWarmDue(w wbWarm, now time.Time) (bool, bool) {
	if w.GaveUp {
		return false, false
	}
	// only a frozen account can be given up on: one that answers is
	// asked again every watch period, and its Until is when it answered
	if !w.Healthy && now.After(w.Until.Add(wbWarmBudget)) {
		return false, true // give up
	}
	return !now.Before(w.NextAt), false
}

// wbProbeBody is the smallest chat WorkBuddy's free model answers.
func wbProbeBody(model string) []byte {
	// non-stream, whatever the plugin's own words about it: a streamed
	// request is answered with its head at once and the refusal, if there
	// is one, inside it — the gateway itself saw 429 for the same account
	// this probe read as 200. A non-stream request is refused with the
	// 429 and the reset in it, which is what this is asking.
	b, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
		"stream":     false,
	})
	return b
}

// wbProbe sends one tiny request to the account through its plugin, the
// way a request of the gateway's goes (pluginFetch), and reports what came
// back: 0 when nothing did.
func wbProbe(ctx context.Context, providerID, account, model string) (int, error) {
	if wbProbeURL() == "" {
		return 0, fmt.Errorf("the gateway's address isn't known yet")
	}
	// the account rests until the moment this is asking about: a request
	// pinned to it is turned away at the gateway without being sent, which
	// would read as a refusal the vendor never made
	wbWake(providerID, account, model)
	ctx, cancel := context.WithTimeout(ctx, wbWarmProbeTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wbProbeURL()+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Magpie-Account", account)
	// magpie's own call, as the usage log reads one: a probe is not the
	// account's own use of its window, and the Usage page counts the two
	// apart (workbuddy_card.go)
	req.Header.Set("User-Agent", "magpie-workbuddy-warmup/1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	log.Printf("workbuddy warm-up: probe %s %s -> %d: %.200s", account, model, resp.StatusCode, strings.TrimSpace(string(b)))
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	return resp.StatusCode, nil
}

// wbStreamRefused reads a stream for a refusal inside it: the vendor's
// error frame, or the words its out-of-quota refusal uses. Empty when the
// stream carries a reply.
func wbStreamRefused(body []byte) string {
	if bytes.Contains(body, []byte("exceeds frequency limit")) ||
		bytes.Contains(body, []byte("usage will reset at")) ||
		bytes.Contains(body, []byte("用量已用完")) {
		return strings.TrimSpace(string(body))
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var f struct {
			Error *struct {
				Message string `json:"message"`
				Code    any    `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &f) == nil && f.Error != nil {
			return f.Error.Message
		}
	}
	return ""
}

// wbWarmNow looks at every kept account and probes the ones due, with
// send the probe. A 200 has the account counted back in at once
// (renewedNow, which the gateway's renewed hook hears) and its entry
// dropped; anything else waits a random while, until the budget is spent.
func wbWarmNow(ctx context.Context, now time.Time, send func(ctx context.Context, providerID, account, model string) (int, error)) {
	wbWarmMu.Lock()
	if wbWarmMu.state == nil {
		wbWarmMu.state = readWBWarmState(wbWarmPath())
	}
	due := make([]wbWarm, 0, len(wbWarmMu.state))
	for _, w := range wbWarmMu.state {
		yes, spent := wbWarmDue(w, now)
		if spent {
			w.GaveUp = true
			wbWarmMu.state[wbKey(w.User, w.Model)] = w
			log.Printf("workbuddy warm-up: %s isn't back within %s of its reset (%s); not asking again", w.User, wbWarmBudget, w.Until.Format(time.RFC3339))
			continue
		}
		if yes {
			due = append(due, w)
		}
	}
	writeWBWarmState(wbWarmPath(), wbWarmMu.state)
	wbWarmMu.Unlock()

	for _, w := range due {
		model := w.Model
		if model == "" {
			model = wbProbeModel
		}
		key := wbKey(w.User, model)
		frozen := !w.Healthy
		began := time.Now()
		c, cancel := context.WithTimeout(ctx, wbWarmProbeTimeout+time.Second)
		status, err := send(c, w.Provider, w.User, model)
		cancel()
		// what it answered, for the Custom page's list: written before the
		// state, so a row there is never older than the state beside it
		rec := wbRecord{At: began, User: w.User, Model: model, Status: status, Ms: time.Since(began).Milliseconds(), Frozen: frozen}
		if err != nil {
			rec.Why = wbOneLine(err.Error())
		}
		addWBRecord(rec)

		wbWarmMu.Lock()
		cur, found := wbWarmMu.state[key]
		if !found {
			// lifted while this was out: what it says is about an account
			// that is being watched no more
			wbWarmMu.Unlock()
			continue
		}
		switch {
		case status == http.StatusOK:
			// still watched: a 200 says the account answers now, not that
			// it will keep answering, and one used up by another
			// computer's request would otherwise never be seen again
			cur.Attempts, cur.Err, cur.Status = 0, "", 0
			cur.Healthy = true
			cur.Until = now
			every := wbEveryOf(cur)
			cur.NextAt = now.Add(every + wbWarmJitter(wbWarmFirst))
			wbWarmMu.state[key] = cur
			writeWBWarmState(wbWarmPath(), wbWarmMu.state)
			wbWarmMu.Unlock()
			renewedNow("plugin:"+w.Provider, w.User)
			log.Printf("workbuddy warm-up: %s answers again; watched again in %s", w.User, every)
			continue
		case err != nil && status == 0:
			// no answer at all: the account is not known to be refused
			cur.Err, cur.Status = err.Error(), 0
		default:
			// a refusal comes back as its status and its words together
			cur.Err, cur.Status = "", status
			if err != nil {
				cur.Err = err.Error()
			}
		}
		cur.Attempts++
		cur.Healthy = false
		// a refusal that names a reset is the vendor's latest word on
		// when the window starts over, and is taken over the one kept:
		// the same account was given 01:29 and answered at 07:12
		if d := wbResets([]byte(cur.Err), now); d > 0 {
			cur.Until = now.Add(d)
		}
		// nothing is asked before the reset it is resting until: asking
		// a frozen account every quarter of an hour for a day told it
		// nothing the reset had not
		if retry := now.Add(wbWarmRetry()); retry.Before(cur.Until) {
			cur.NextAt = cur.Until.Add(wbWarmJitter(wbWarmFirst))
		} else {
			cur.NextAt = retry
		}
		wbWarmMu.state[key] = cur
		writeWBWarmState(wbWarmPath(), wbWarmMu.state)
		wbWarmMu.Unlock()
		log.Printf("workbuddy warm-up: %s answered %d%s; asking again at %s (reset %s)", w.User, status, wbWarmWhy(cur.Err), cur.NextAt.Format(time.RFC3339), cur.Until.Format(time.RFC3339))
	}
}

// wbOneLine folds what a refusal said onto one line, for a row that shows
// it: the words are a JSON body with the vendor's sentence inside.
func wbOneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}

// wbWarmWhy is err as the log says it, "" for none.
func wbWarmWhy(err string) string {
	if err == "" {
		return ""
	}
	return " (" + err + ")"
}

// WorkBuddyWarmNow is one look, for the app and a test: what wbWarmNow
// does with the real probe.
func WorkBuddyWarmNow(ctx context.Context, now time.Time) {
	wbWarmNow(ctx, now, wbProbe)
}

// KeepWorkBuddyWarm probes the frozen WorkBuddy accounts, every
// wbWarmEvery until ctx ends. The wall clock is looked at each time: a
// machine that slept through a reset notices on waking.
func KeepWorkBuddyWarm(ctx context.Context) {
	t := time.NewTicker(wbWarmEvery)
	defer t.Stop()
	watched := time.Now().Add(-wbWatchEvery) // due at the first tick
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			// every account is looked for, not only the frozen ones: one
			// used up by another computer's request never came through
			// failureOf here, and would sit out unnoticed
			if now.Sub(watched) >= wbWatchEvery {
				watched = now
				wbWatch(ctx, now)
			}
			wbWarmNow(ctx, now, wbProbe)
		}
	}
}

// wbRecord is one probe as the Custom page lists it: when it went, what
// came back, and the account and model it asked about.
type wbRecord struct {
	At     time.Time `json:"at"`
	User   string    `json:"user"`
	Model  string    `json:"model"`
	Status int       `json:"status"`
	// Why is what a refusal said, folded to one line; empty on a 200.
	Why string `json:"why,omitempty"`
	// Ms is how long the probe took.
	Ms int64 `json:"ms,omitempty"`
	// Frozen is whether the account was used up before this probe, which
	// tells the warm-up's asking from the watching of one that answers.
	Frozen bool `json:"frozen,omitempty"`
}

// wbRecordKeep is how many probes are kept: a day's worth at the default
// interval, over every account and model, is well inside it.
const wbRecordKeep = 2000

var wbRecMu struct {
	sync.Mutex
	recs []wbRecord
}

func wbRecordPath() string { return filepath.Join(filepath.Dir(Path()), "workbuddy-probes.json") }

// readWBRecords reads the kept probes, none when they can't be read.
func readWBRecords(path string) []wbRecord {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var recs []wbRecord
	if json.Unmarshal(b, &recs) != nil {
		return nil
	}
	return recs
}

// addWBRecord keeps one probe, newest last, the oldest dropped past
// wbRecordKeep, and writes them out.
func addWBRecord(rec wbRecord) {
	wbRecMu.Lock()
	if wbRecMu.recs == nil {
		wbRecMu.recs = readWBRecords(wbRecordPath())
	}
	wbRecMu.recs = append(wbRecMu.recs, rec)
	if over := len(wbRecMu.recs) - wbRecordKeep; over > 0 {
		wbRecMu.recs = wbRecMu.recs[over:]
	}
	recs := append([]wbRecord(nil), wbRecMu.recs...)
	wbRecMu.Unlock()

	if b, err := json.Marshal(recs); err == nil {
		dir := filepath.Dir(wbRecordPath())
		os.MkdirAll(dir, 0o755)
		tmp := wbRecordPath() + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			os.Rename(tmp, wbRecordPath())
		}
	}
}

// WorkBuddyProbes is every kept probe, newest first, the account's own
// when user isn't empty, at most limit of them.
func WorkBuddyProbes(user, model string, limit int) []wbRecord {
	wbRecMu.Lock()
	if wbRecMu.recs == nil {
		wbRecMu.recs = readWBRecords(wbRecordPath())
	}
	recs := wbRecMu.recs
	wbRecMu.Unlock()

	out := make([]wbRecord, 0, min(limit, len(recs)))
	for i := len(recs) - 1; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		r := recs[i]
		if user != "" && !strings.EqualFold(r.User, user) {
			continue
		}
		if model != "" && r.Model != model {
			continue
		}
		out = append(out, r)
	}
	return out
}

// ResetWorkBuddyProbes drops the kept probes, for a test.
func ResetWorkBuddyProbes() {
	wbRecMu.Lock()
	defer wbRecMu.Unlock()
	wbRecMu.recs = []wbRecord{}
}

// WorkBuddyWarmed is the kept state, for the app and a test.
func WorkBuddyWarmed() map[string]wbWarm {
	wbWarmMu.Lock()
	defer wbWarmMu.Unlock()
	if wbWarmMu.state == nil {
		wbWarmMu.state = readWBWarmState(wbWarmPath())
	}
	out := make(map[string]wbWarm, len(wbWarmMu.state))
	for k, w := range wbWarmMu.state {
		out[k] = w
	}
	return out
}

// ResetWorkBuddyWarmed drops the kept state, for a test.
func ResetWorkBuddyWarmed() {
	wbWarmMu.Lock()
	defer wbWarmMu.Unlock()
	wbWarmMu.state = wbWarmState{}
}
