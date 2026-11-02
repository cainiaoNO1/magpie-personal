package provider

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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/catalog"
)

// Antigravity bills its accounts' models against pools of their own:
// "Gemini", "Claude & GPT", and on some plans a "Gemini 4 Argon". Each pool
// has a 5-hour window and a 7-day one, or — measured on a live account —
// Argon has the 7-day one alone. A pool's 5-hour window is not a rolling
// one: it starts at the first request that draws on it and ends 5 hours
// after that, so an account nobody calls keeps it for whenever the user
// does. Self-use therefore wants a first call made early in every pool:
// with one already made, the hours the user's own work spends are the
// window's own, and the reset lands at the time of day the user is at the
// keyboard rather than wherever a rolling window would have put it.
//
// So this looks at every pool of every account on a schedule and, when that
// pool's clock has not started, sends exactly one tiny request through the
// gateway, pinned to that account, with a model of that pool: which model
// draws on which pool is measured, and is what the pool constants below
// name. A pool already drawing on itself is left alone, and the wait
// between looks is random within the last fifth of the interval the user
// set, which keeps a day's looks from settling onto a fixed time.
//
// A pool is read by its governing window: its 5 hours where it has one,
// else its 7 days. Measured on a live account, "Gemini 4 Argon" reports a
// weekly bucket and no 5-hour one at all, so a pool read by its 5 hours
// alone is one that would never be opened. The other windows are read
// beside it for the page to show.
//
// A pool magpie knows no model of — one the vendor adds after this was
// written — is shown on the page and never called.

// The pools the watching opens, as poolName names them from Antigravity's
// group names ("Gemini Models", "Claude and GPT models").
const (
	gemPool       = "Gemini"
	gemClaudePool = "Claude & GPT"
	gemArgonPool  = "Gemini 4 Argon"
)

// gemArgonFamily is the Argon pool's model as magpie offers it. Measured on
// a live account, a call to gemini-4-argon moved that pool's 7-day figure
// 0.2332 -> 0.2345 and upstream answered as model "gemini-4-argon-trial",
// which is on no account's list. The id the pool carries is -disabled on the
// same account, and a call to that is no call at all.
const gemArgonFamily = "gemini-4-argon"

// gemFlashFamily is the one Gemini family whose calls move that pool's
// 5-hour counter. Measured on a live account: 2157 tokens sent to
// gemini-2.5-flash-lite, 991 to it again, 1167 to gemini-3-flash and 11 to
// gemini-3.5-flash-lite left the counter where it was, and so did a 12-token
// gemini-3.8-flash call — while a 132-token one at max_tokens=1 moved it. The
// model decides it, not the size of the call, and of the families tried only
// gemini-3.8-flash (and its -tiered id) draws on the pool. The same call
// left "Gemini 4 Argon" at 0.2345, so the two pools are opened apart.
const gemFlashFamily = "gemini-3.8-flash"

// gemWarmEvery is how often the loop looks at the clock. It is not the
// interval between looks — those are the user's, gemWait apart — but how
// finely a look that has come due is noticed.
const gemWarmEvery = 20 * time.Second

// gemWatchEvery is the longest wait between two looks when the user sets no
// interval of their own.
const gemWatchEvery = 5 * time.Minute

// gemWatchMin is the shortest wait the page may set. Every look that finds
// a pool unused spends a request, and the vendor's quota endpoint is
// rate limited: asking faster than this is more than the watching is worth.
const gemWatchMin = time.Minute

// gemScanEvery is how often the accounts are listed again, for one signed in
// since the last time.
const gemScanEvery = 30 * time.Minute

// gemProbeTimeout bounds one first call. Flash answers at once; a call still
// unanswered after this is not worth holding the loop for.
const gemProbeTimeout = 60 * time.Second

// gemPoolWarm is what is kept of one pool between looks: when it was last
// read, when it is read again, and what its last first call answered. A pool
// apart rather than an account, because an account's pools open on clocks of
// their own: the Argon pool's first call is its 7-day window's, where the
// Gemini pool's is its 5-hour one.
type gemPoolWarm struct {
	Pool       string    `json:"pool"`
	Model      string    `json:"model,omitempty"`
	LastCheck  time.Time `json:"lastCheck,omitzero"`
	NextAt     time.Time `json:"nextAt,omitzero"`
	LastCallAt time.Time `json:"lastCallAt,omitzero"`
	// Attempts is how many first calls have been sent for this pool since
	// magpie began watching it, refused ones included: a refusal is still a
	// request the account saw.
	Attempts int    `json:"attempts,omitempty"`
	Status   int    `json:"status,omitempty"`
	Err      string `json:"err,omitempty"`
}

// gemWarm is what is kept of one account between looks: when it was last
// read, when it is read again, what went wrong with the reading itself, and
// what is kept of each of its pools, by pool name.
type gemWarm struct {
	User      string                 `json:"user"`
	LastCheck time.Time              `json:"lastCheck,omitzero"`
	NextAt    time.Time              `json:"nextAt,omitzero"`
	Err       string                 `json:"err,omitempty"`
	Pools     map[string]gemPoolWarm `json:"pools,omitempty"`
}

// poolState is what is kept of one of the account's pools, the zero one for
// a pool looked at for the first time.
func (w gemWarm) poolState(pool string) gemPoolWarm {
	if ps, ok := w.Pools[pool]; ok {
		return ps
	}
	return gemPoolWarm{Pool: pool}
}

// setPool keeps what this look made of one pool.
func (w *gemWarm) setPool(ps gemPoolWarm) {
	if w.Pools == nil {
		w.Pools = map[string]gemPoolWarm{}
	}
	w.Pools[ps.Pool] = ps
}

var gemWarmMu struct {
	sync.Mutex
	// state is keyed by the account's user, lowercased.
	state map[string]gemWarm
	// read is the last reading of every account's allowance, kept for the
	// page: the windows it shows are the vendor's own figures, and a page
	// opened between two looks has them without asking the vendor again.
	read map[string]SubscriptionQuota
	// scanned is when the accounts were last listed.
	scanned time.Time
}

// gemWarmPath is where the watching is kept, beside the providers' own file.
func gemWarmPath() string {
	return filepath.Join(filepath.Dir(Path()), "gemini-warmup.json")
}

// readGemWarmState is what was kept, empty for a file that isn't there or
// can't be read: the watching starts over rather than refusing to run. A
// file written before the pools were kept apart reads with none of them, and
// the next look gives each pool its own.
func readGemWarmState(path string) map[string]gemWarm {
	out := map[string]gemWarm{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	if json.Unmarshal(b, &out) != nil {
		return map[string]gemWarm{}
	}
	return out
}

// writeGemWarmState keeps st, written whole and renamed into place so a
// reader never sees half of one.
func writeGemWarmState(path string, st map[string]gemWarm) {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	os.Rename(tmp, path)
}

// GeminiWatch is how the user wants the watching done.
type GeminiWatch struct {
	On bool `json:"on,omitempty"`
	// Every is the longest wait between two looks. The wait is random within
	// its last fifth, so a day's looks don't settle onto one time; zero is
	// gemWatchEvery.
	Every time.Duration `json:"every,omitempty"`
}

var gemWatchMu struct {
	sync.Mutex
	set GeminiWatch
}

// GeminiWatchOf is a setting as the watching reads it: an interval below
// gemWatchMin is taken as none set, so the page can't make the watching ask
// faster than the vendor's endpoint is worth asking.
func GeminiWatchOf(on bool, every time.Duration) GeminiWatch {
	if every < gemWatchMin {
		every = 0
	}
	return GeminiWatch{On: on, Every: every}
}

// SetGeminiWatch is the watching the user set, from the page or at startup.
func SetGeminiWatch(w GeminiWatch) {
	gemWatchMu.Lock()
	gemWatchMu.set = w
	gemWatchMu.Unlock()
}

// GeminiWatchMin is the shortest interval the page may set.
func GeminiWatchMin() time.Duration { return gemWatchMin }

// GeminiWatchDefault is the wait when the user sets none, for the page to say
// what zero is.
func GeminiWatchDefault() time.Duration { return gemWatchEvery }

// gemWatched says whether the user asked for the watching at all.
func gemWatched() bool {
	gemWatchMu.Lock()
	defer gemWatchMu.Unlock()
	return gemWatchMu.set.On
}

// gemEvery is the longest wait between two looks, as set.
func gemEvery() time.Duration {
	gemWatchMu.Lock()
	defer gemWatchMu.Unlock()
	if gemWatchMu.set.Every >= gemWatchMin {
		return gemWatchMu.set.Every
	}
	return gemWatchEvery
}

// gemWait is how long to wait before the next look: random within the last
// fifth of the interval, so the looks a day holds land at times the user's
// own work does not always meet.
func gemWait() time.Duration {
	every := gemEvery()
	return every - time.Duration(rand.Int64N(int64(every/5)+1))
}

// gemProbeURL is where the gateway listens, as a client reaches it: the
// gateway sets it at startup (SetGeminiProbeURL). Empty until then, and a
// first call with nowhere to go is skipped rather than sent into nothing.
var gemProbeURL = func() string { return "" }

// SetGeminiProbeURL says where the gateway listens, so a first call goes
// through it as a real request does: routed, pinned to its account, counted
// in the usage log, and refused here rather than sent when the account is
// resting.
func SetGeminiProbeURL(u string) {
	gemProbeURL = func() string { return strings.TrimRight(u, "/") }
}

// gemModels is the models one Antigravity account lists, its own list first
// and the provider's pooled one when that is all there is.
func gemModels(user string) []catalog.Model {
	ms, _, ok := catalog.Live(accountModels("antigravity", user))
	if ok && len(ms) > 0 {
		return ms
	}
	ms, _, _ = catalog.Live("antigravity")
	return ms
}

// gemModelsOfTheReading are the models the allowance itself named, by id and
// name. The first look of a process runs a second or two after start
// (KeepGeminiWarm looks at once), where the catalogue may not have been read
// yet, and a reading taken before it would find no model for a pool the plan
// does list. A reading names no price, so these come last.
func gemModelsOfTheReading(q SubscriptionQuota) []catalog.Model {
	var out []catalog.Model
	for _, w := range q.Windows {
		if w.Model == "" {
			continue
		}
		out = append(out, catalog.Model{ID: w.Model, Name: w.Name})
	}
	return out
}

// gemModelFor is the model a first call for one of the account's pools asks,
// "" for a pool no model of the account's list draws on. A model that draws
// on no pool of the one it is asked for starts no clock: the call comes back
// 200, the window stays unused, and the watching sends another first call
// every look for as long as the account is idle. Which model draws on which
// pool is measured, and the account's own list is read rather than a name
// written here: a plan that doesn't list the model asked for answers with a
// refusal ("X-Magpie-Account: y19913868365@gmail.com's plan doesn't list
// claude-sonnet-4-6", measured), which starts no clock either. The reading
// the look took is the second source, and only where the catalogue names no
// model of that pool at all: it is what tells the pool's models before the
// catalogue has been read.
func gemModelFor(user, pool string, q SubscriptionQuota) string {
	if model := gemModelIn(gemModels(user), pool); model != "" {
		return model
	}
	return gemModelIn(gemModelsOfTheReading(q), pool)
}

// gemModelIn is the model of one pool among ms, "" when none of them draws on
// it.
func gemModelIn(ms []catalog.Model, pool string) string {
	if len(ms) == 0 {
		return ""
	}
	// Antigravity's own list names a model's levels apart
	// ("gemini-3.7-flash-low"), and magpie offers the family: the family id
	// is what a request names, and what the gateway resolves.
	byID := make(map[string]catalog.Model, len(ms))
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		id := m.ID
		if base, _, ok := antigravityBaseIn(ms, id); ok {
			id = base
		}
		if id == "" || byID[id].ID != "" {
			continue
		}
		m.ID = id
		byID[id] = m
		ids = append(ids, id)
	}
	switch pool {
	case gemPool:
		return gemGeminiModel(byID, ids)
	case gemClaudePool:
		return gemClaudeModel(byID, ids)
	case gemArgonPool:
		return gemArgonModel(byID, ids)
	}
	return ""
}

// gemGeminiModel is the Gemini pool's first call: gemFlashFamily where the
// account lists it, else the cheapest flash-lite it lists. Cheap is not what
// decides this one. The cheapest flash-lite — the obvious choice, and the
// one this made until the count of first calls gave it away — is the wrong
// one here: it answers 200 and draws on nothing.
func gemGeminiModel(byID map[string]catalog.Model, ids []string) string {
	gem := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.HasPrefix(strings.ToLower(id), "gemini") {
			gem = append(gem, id)
		}
	}
	if len(gem) == 0 {
		return ""
	}
	// a variant of the family is asked for as the family, and a level of it
	// (-low, -medium) is never asked for itself — measured, those answer with
	// no usage at all. The -tiered id is the same family at the level a
	// request names, and is what a plan listing only it is asked for.
	for _, want := range []string{gemFlashFamily, gemFlashFamily + "-tiered"} {
		if m, ok := byID[want]; ok {
			return m.ID
		}
	}
	// no 3.8 family on this plan: the cheapest flash there is, as this chose
	// before, so an older plan still gets a call — and still starts a window,
	// if one of these turns out to draw on the pool after all. None of them
	// was measured to.
	for _, want := range []string{"gemini-2.5-flash-lite", "flash-lite", "flash"} {
		if picked := gemWith(byID, gem, want); len(picked) > 0 {
			// the cheapest of them by its list price: 3.1-flash-lite costs
			// more than 2.5-flash-lite where models.dev lists both
			return warmPick(picked, func(id string) (catalog.Price, bool) {
				return catalog.PricedBy([]string{"google"}, id)
			}).ID
		}
	}
	return gem[0]
}

// gemClaudeModel is the Claude & GPT pool's first call: a sonnet where the
// account lists one, else any claude, else a gpt-oss, the cheapest of the
// first kind that is there. Measured, claude-sonnet-4-6 moved that pool's
// 5-hour figure 0 -> 0.0015 and pinned both of its windows at call time + 5
// hours and + 7 days, where a gemini-3.8-flash call left the pool where it
// was. Which sonnet a plan has is the plan's own business: the pix account
// refuses claude-sonnet-4-6 and lists claude-sonnet-5-5 instead, so the
// name is read off the account's list rather than written here.
func gemClaudeModel(byID map[string]catalog.Model, ids []string) string {
	for _, want := range []string{"sonnet", "claude", "gpt-oss"} {
		if picked := gemWith(byID, ids, want); len(picked) > 0 {
			return warmPick(picked, func(id string) (catalog.Price, bool) {
				return catalog.PricedBy([]string{"google"}, id)
			}).ID
		}
	}
	return ""
}

// gemArgonModel is the Argon pool's first call: the family itself where the
// account lists it, else a variant of it, the -disabled id it carries on
// some accounts being no call at all.
func gemArgonModel(byID map[string]catalog.Model, ids []string) string {
	if m, ok := byID[gemArgonFamily]; ok {
		return m.ID
	}
	for _, id := range ids {
		if strings.HasPrefix(id, gemArgonFamily) && id != gemArgonFamily+"-disabled" {
			return byID[id].ID
		}
	}
	return ""
}

// gemWith are the models of ids that have word in their id, in the order the
// account lists them.
func gemWith(byID map[string]catalog.Model, ids []string, word string) []catalog.Model {
	var out []catalog.Model
	for _, id := range ids {
		if strings.Contains(strings.ToLower(id), word) {
			out = append(out, byID[id])
		}
	}
	return out
}

// gemPoolWatched says whether a pool is one this knows a model of: a pool
// the vendor adds after this was written is shown on the page and never
// called, since a call to a model that draws on another pool opens that one
// instead.
func gemPoolWatched(pool string) bool {
	switch pool {
	case gemPool, gemClaudePool, gemArgonPool:
		return true
	}
	return false
}

// gemPoolWindows is one of an account's pools and the windows it reports,
// either nil when the reading has none.
type gemPoolWindows struct {
	Pool string
	Five *QuotaWindow
	Week *QuotaWindow
}

// governing is the window whose own figure says whether the pool's clock is
// running: the 5-hour one where the pool has one, else the 7-day one. A pool
// with the weekly window alone is read by that one alone — measured, "Gemini
// 4 Argon" reports no 5-hour bucket at all — and a pool is open on its
// governing window's own use, not on any of its others: a 5-hour window that
// has reset to nothing is not running because its 7-day window still carries
// use.
func (p gemPoolWindows) governing() *QuotaWindow {
	if p.Five != nil {
		return p.Five
	}
	return p.Week
}

// windows are the pool's own windows as the page draws them: the 5 hours
// first, the 7 days second, whichever of them the pool has, and each left
// out where the vendor gave no figure for it.
func (p gemPoolWindows) windows() []QuotaWindow {
	out := make([]QuotaWindow, 0, 2)
	for _, w := range []*QuotaWindow{p.Five, p.Week} {
		if w != nil && gemRead(*w) {
			out = append(out, *w)
		}
	}
	return out
}

// gemPools are the pools one account's allowance reports, each with the
// windows it has, by pool name. A plan with two pools has two of these and
// one carrying Argon has three.
func gemPools(q SubscriptionQuota) []gemPoolWindows {
	byName := map[string]*gemPoolWindows{}
	var names []string
	at := func(pool string) *gemPoolWindows {
		if p, ok := byName[pool]; ok {
			return p
		}
		p := &gemPoolWindows{Pool: pool}
		byName[pool] = p
		names = append(names, pool)
		return p
	}
	for i := range q.Windows {
		w := &q.Windows[i]
		if w.Model != "" || w.Pool == "" {
			continue
		}
		switch windowSpan(*w) {
		case modelSpan:
			if p := at(w.Pool); p.Five == nil {
				p.Five = w
			}
		case 7 * 24 * time.Hour:
			if p := at(w.Pool); p.Week == nil {
				p.Week = w
			}
		}
	}
	// the summary of groups couldn't be read (pools, google.go): each model
	// of a group carries the group's 5-hour figure, the tightest of them
	// being the group's own
	if p, ok := byName[gemPool]; !ok || p.Five == nil {
		var tight *QuotaWindow
		for i := range q.Windows {
			w := &q.Windows[i]
			if w.Model == "" || !strings.EqualFold(w.Family, gemPool) {
				continue
			}
			if w.Span != 0 && w.Span != modelSpan {
				continue
			}
			if tight == nil || w.Used > tight.Used {
				tight = w
			}
		}
		if tight != nil {
			if p := at(gemPool); p.Five == nil {
				p.Five = tight
			}
		}
	}
	out := make([]gemPoolWindows, 0, len(names))
	for _, name := range names {
		out = append(out, *byName[name])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pool < out[j].Pool })
	return out
}

// gemRead says whether w carries a figure of the vendor's own. A group's
// bucket is made with Used 100 (pools, google.go) and given the summary's
// figure only when it has a remainingFraction, so one left without it says
// 100 — the number a window used up says — and is told from a real one by
// having no reset beside it. A model's own window is a figure by
// construction (fetchAvailableModels leaves out the ones it has none for),
// so only a bucket is doubted.
func gemRead(w QuotaWindow) bool {
	return w.Model != "" || w.Pool == "" || w.ResetsAt != nil || w.ResetSecs > 0
}

// GeminiPool is one pool of an Antigravity account as the Gemini page shows
// it: its own windows, the model its first call asks, and what the last look
// made of it.
type GeminiPool struct {
	Pool string `json:"pool"`
	// Watched is false for a pool magpie knows no model of: drawn, never
	// called.
	Watched bool `json:"watched"`
	// Windows are this pool's own: its 5 hours and then its 7 days, whichever
	// of them it has.
	Windows    []QuotaWindow `json:"windows"`
	Model      string        `json:"model,omitempty"`
	LastCheck  time.Time     `json:"lastCheck,omitzero"`
	NextAt     time.Time     `json:"nextAt,omitzero"`
	LastCallAt time.Time     `json:"lastCallAt,omitzero"`
	Attempts   int           `json:"attempts,omitempty"`
	Status     int           `json:"status,omitempty"`
	Err        string        `json:"err,omitempty"`
}

// GeminiRow is one Antigravity account as the Gemini page shows it, with
// every pool it has.
type GeminiRow struct {
	User string `json:"user"`
	// Pools are the account's pools, by name, each with the windows the last
	// look read of it.
	Pools     []GeminiPool `json:"pools"`
	LastCheck time.Time    `json:"lastCheck,omitzero"`
	NextAt    time.Time    `json:"nextAt,omitzero"`
	Err       string       `json:"err,omitempty"`
}

// RefreshGemini reads the Antigravity accounts' allowances now, as the page's
// Refresh asks for, and keeps them for the page: the look of its own is
// minutes away, and a reading taken less than a minute ago is a snapshot from
// before a window reset. The accounts are listed as watched even before the
// first look, so a page has them to draw while the watching is off.
func RefreshGemini(ctx context.Context, now time.Time) {
	forgetGeminiReadings()
	usage := LoginUsage(Asked(ctx), "antigravity")
	gemWarmMu.Lock()
	if gemWarmMu.state == nil {
		gemWarmMu.state = readGemWarmState(gemWarmPath())
	}
	for user := range usage {
		key := strings.ToLower(user)
		st, seen := gemWarmMu.state[key]
		if !seen {
			st = gemWarm{User: user}
		}
		st.User = user
		gemWarmMu.state[key] = st
	}
	gemWarmMu.read = usage
	gemWarmMu.Unlock()
	if gemWatched() {
		// the look of its own is minutes away, and Refresh is the user asking
		// for it now: a pool the fresh reading shows as not started is opened
		// here rather than at the next tick
		gemWarmNow(ctx, now)
	}
}

// GeminiRows is every Antigravity account the watching knows of, with its
// pools and the allowance it last read. Nothing is asked of the vendor here:
// a page opened between two looks shows the last one.
func GeminiRows(now time.Time) []GeminiRow {
	gemWarmMu.Lock()
	defer gemWarmMu.Unlock()
	if gemWarmMu.state == nil {
		gemWarmMu.state = readGemWarmState(gemWarmPath())
	}
	watched := gemWatched()
	// a look that has come due is made at the loop's next tick, and nothing
	// is scheduled while the watching is off
	when := func(at time.Time) time.Time {
		switch {
		case !watched:
			return time.Time{}
		case !at.IsZero() && at.Before(now):
			return now
		}
		return at
	}
	rows := make([]GeminiRow, 0, len(gemWarmMu.state))
	for key, st := range gemWarmMu.state {
		row := GeminiRow{User: st.User, LastCheck: st.LastCheck, NextAt: when(st.NextAt), Err: st.Err}
		if q, ok := gemWarmMu.read[key]; ok {
			for _, p := range gemPools(q) {
				ps := st.poolState(p.Pool)
				row.Pools = append(row.Pools, GeminiPool{
					Pool:       p.Pool,
					Watched:    gemPoolWatched(p.Pool),
					Windows:    p.windows(),
					Model:      ps.Model,
					LastCheck:  ps.LastCheck,
					NextAt:     when(ps.NextAt),
					LastCallAt: ps.LastCallAt,
					Attempts:   ps.Attempts,
					Status:     ps.Status,
					Err:        ps.Err,
				})
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].User < rows[j].User })
	return rows
}

// gemProbe sends one first call to the account through the gateway, as a
// request of a client's goes: routed, pinned to that account by the header
// the gateway reads (AccountHeader), counted in the usage log. The reply is
// not kept — this asks for a clock to be started, not for an answer.
func gemProbe(ctx context.Context, account, model string) (int, error) {
	url := gemProbeURL()
	if url == "" {
		return 0, fmt.Errorf("the gateway's address isn't known yet")
	}
	ctx, cancel := context.WithTimeout(ctx, gemProbeTimeout)
	defer cancel()
	// non-stream: a streamed request is answered with its head at once and a
	// refusal inside it, where a request answered whole carries the status
	// the gateway gave it.
	//
	// max_tokens is sent as a client's would be, and Antigravity's Gemini
	// drops it: buildCodeAssistSent writes maxOutputTokens only for a
	// vendor that isn't Antigravity or for its Claude (gateway/codeassist.go:
	// "if r.MaxTokens > 0 && (!ag || claude)"), and a first call measured
	// against the live gateway came back with completion_tokens 10 for
	// max_tokens 1. What keeps the reply short is the prompt, one word; the
	// field is sent so the call is shaped like any client's, not because it
	// bounds anything here.
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
		"stream":     false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Magpie-Account", account) // gateway.AccountHeader, which this package can't name
	// magpie's own call, as the usage log reads one: the page counts the
	// warm-up's requests apart from the account's own use
	req.Header.Set("User-Agent", "magpie-gemini-warmup/1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	log.Printf("gemini warm-up: first call %s %s -> %d: %.200s", account, model, resp.StatusCode, strings.TrimSpace(string(b)))
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("%s", gemWhy(string(b)))
	}
	return resp.StatusCode, nil
}

// gemWhy is a refusal as the page says it: one line, bounded.
func gemWhy(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// forgetGeminiReadings has the Antigravity accounts' next reading asked for
// rather than taken from the minute's cache, which holds a reading taken
// before a window reset as if it were now.
func forgetGeminiReadings() {
	for _, l := range googleLogins("antigravity") {
		forgetLoginReading(l.Login)
	}
}

// gemResetSince says whether any window of the last reading has reset since
// it was taken. A look made on that reading reads a window that has started
// over as one already in use, so it would wait another interval for nothing.
func gemResetSince(read map[string]SubscriptionQuota, now time.Time) bool {
	for _, q := range read {
		for _, w := range q.Windows {
			if w.ResetsAt != nil && !now.Before(*w.ResetsAt) {
				return true
			}
		}
	}
	return false
}

// gemDue says whether any account is to be looked at now, and whether the
// accounts are to be listed again for one signed in since.
func gemDue(state map[string]gemWarm, now, scanned time.Time) (due, rescan bool) {
	if scanned.IsZero() || now.Sub(scanned) >= gemScanEvery {
		rescan = true
	}
	for _, st := range state {
		if !now.Before(st.NextAt) {
			return true, rescan
		}
	}
	return false, rescan
}

// gemWarmNow is one look: every account is read, and every pool of it whose
// clock has not started is given one first call of its own. A reading that
// failed is kept with what it said and looked at again next time — never in
// a spin, as every look waits gemWait.
func gemWarmNow(ctx context.Context, now time.Time) {
	if !gemWatched() {
		return
	}
	gemWarmMu.Lock()
	if gemWarmMu.state == nil {
		gemWarmMu.state = readGemWarmState(gemWarmPath())
	}
	due, rescan := gemDue(gemWarmMu.state, now, gemWarmMu.scanned)
	gemWarmMu.Unlock()
	if !due && !rescan {
		return
	}
	// a window of the last reading that has reset since it was taken is not
	// what the minute's cache would hand back: it says the pool is in use, so
	// the look that finds it started over would open it a whole interval
	// later. Reading it afresh is what makes a reset noticed at the tick it
	// happens on.
	gemWarmMu.Lock()
	reset := gemResetSince(gemWarmMu.read, now)
	gemWarmMu.Unlock()
	if reset {
		forgetGeminiReadings()
	}
	// read as one the user asked for: with Settings → Usage at "read
	// allowances only when I ask", a read magpie makes on its own comes back
	// as what was read last (cachedLoginReading), with no error saying so,
	// and "nothing used" from a stale reading would spend a first call every
	// look — the opposite of what this is for. A warm-up isn't a reading the
	// user is shown, and the docs say the same (docs/subsystems/
	// providers-accounts.md: "Warm-up and check-in are not reads and are
	// unchanged"); codex_warmup.go reads unasked, and a Codex window is known
	// by its reset time, which a stale reading still carries, where an
	// Antigravity window's "nothing used" is the whole fact
	usage := LoginUsage(Asked(ctx), "antigravity")
	type firstCall struct{ user, pool, model string }
	var calls []firstCall
	gemWarmMu.Lock()
	for user, q := range usage {
		key := strings.ToLower(user)
		st, seen := gemWarmMu.state[key]
		if !seen {
			// an account watched for the first time is looked at at once: the
			// sooner its clocks start the sooner its hours are the user's
			st = gemWarm{User: user}
		}
		st.User = user
		if now.Before(st.NextAt) {
			gemWarmMu.state[key] = st
			continue
		}
		st.LastCheck, st.NextAt, st.Err = now, now.Add(gemWait()), ""
		switch {
		case q.Error != "":
			st.Err = gemWhy(q.Error)
		default:
			pools := gemPools(q)
			if len(pools) == 0 {
				st.Err = "no pool is listed for this account"
			}
			for _, p := range pools {
				ps := st.poolState(p.Pool)
				ps.LastCheck, ps.NextAt, ps.Err = now, st.NextAt, ""
				gov := p.governing()
				switch {
				case !gemPoolWatched(p.Pool):
					ps.Err = "this pool isn't watched: magpie knows no model that draws on it"
				case gov == nil:
					ps.Err = "no 5-hour or 7-day window is listed for this pool"
				case !gemRead(*gov):
					// a bucket the summary gave no figure for: pools (google.go)
					// starts each with Used 100, so read as a figure it would say the
					// window is used up when nothing is known of it at all
					ps.Err = "Antigravity reported no figure for this pool's " + gov.Name + " window"
				case gov.Used > 0:
					// the clock is running: this window's hours are the user's own
				case q.AsOf != nil:
					// the reading is one kept from an earlier look, shown because
					// this one couldn't make its own: what it says was true when it
					// was read, and a call made on "nothing used" from it would be
					// made blind
					ps.Err = "this look couldn't read the allowance; last read " + q.AsOf.Format(time.RFC3339)
				default:
					model := gemModelFor(user, p.Pool, q)
					if model == "" {
						ps.Err = "no model of this pool is on the account's list"
						break
					}
					ps.Model = model
					calls = append(calls, firstCall{user, p.Pool, model})
				}
				st.setPool(ps)
			}
		}
		gemWarmMu.state[key] = st
	}
	gemWarmMu.read = usage
	gemWarmMu.scanned = now
	gemWarmMu.Unlock()

	for _, c := range calls {
		status, err := gemProbe(ctx, c.user, c.model)
		gemWarmMu.Lock()
		st := gemWarmMu.state[strings.ToLower(c.user)]
		ps := st.poolState(c.pool)
		ps.Model = c.model
		ps.Status, ps.Attempts = status, ps.Attempts+1
		if err != nil {
			ps.Err = gemWhy(err.Error())
		} else {
			ps.LastCallAt = time.Now()
		}
		st.setPool(ps)
		gemWarmMu.state[strings.ToLower(c.user)] = st
		gemWarmMu.Unlock()
	}

	gemWarmMu.Lock()
	writeGemWarmState(gemWarmPath(), gemWarmMu.state)
	gemWarmMu.Unlock()
}

// KeepGeminiWarm looks at the Antigravity accounts' pools until ctx ends, one
// look every gemWait while the watching is on. The wall clock is read each
// tick, so a machine that slept through a look notices on waking.
func KeepGeminiWarm(ctx context.Context) {
	// one look at once rather than at the first tick: an account whose pool
	// has nothing used yet starts its clock a tick sooner, and a page opened
	// before the first tick has the allowance to show
	gemWarmNow(ctx, time.Now())
	t := time.NewTicker(gemWarmEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			gemWarmNow(ctx, now)
		}
	}
}
