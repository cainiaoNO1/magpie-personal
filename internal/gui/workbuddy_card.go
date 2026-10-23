package gui

import (
	"sort"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

// workbuddyRow is one account and model of WorkBuddy's free models as the
// Usage page draws it: whether the account answers, when its window starts
// over, and how much of the window it has spent.
//
// The vendor counts a free model's allowance in tokens, one window an
// account and a model, and starts a window over 24 hours after the first
// call that used it up. It says when a window it has used up starts over,
// but says nothing of one that is still answering, so the row is read from
// both: the reset WorkBuddy named, and the tokens magpie itself routed
// since that window began (workbuddy_warmup.go).
type workbuddyRow struct {
	Provider string `json:"provider"`
	User     string `json:"user"`
	Model    string `json:"model"`
	// Healthy is whether the last probe was answered: an account that
	// answers is in its window, one that doesn't has used it up.
	Healthy bool `json:"healthy"`
	// Until is when the window starts over, as WorkBuddy named it; zero
	// when it named none and none has been seen since.
	Until *time.Time `json:"until,omitempty"`
	// StartsAt is when the window began, from its reset and its span.
	StartsAt *time.Time `json:"startsAt,omitempty"`
	// FirstCallAt is the first request time of this window (after thaw, or today's earliest).
	FirstCallAt *time.Time `json:"firstCallAt,omitempty"`
	// Spent is what the window has spent, in tokens, of Limit; the two
	// are zero when magpie routed nothing in it.
	Spent int64 `json:"spent"`
	Limit int64 `json:"limit"`
	// Calls is how many requests the window has answered, and Probed how
	// many of those were the warm-up's own, so a window's real use reads
	// apart from the watching.
	Calls  int `json:"calls"`
	Probed int `json:"probed"`
	// Status is what the last probe answered, 0 for a 200 or none yet,
	// and Err what it said when it refused.
	Status int    `json:"status,omitempty"`
	Err    string `json:"err,omitempty"`
	// Attempts is how many probes have been refused since the window was
	// used up, and GaveUp whether the watching has stopped.
	Attempts int  `json:"attempts,omitempty"`
	GaveUp   bool `json:"gaveUp,omitempty"`
}

// workbuddyLimit is a free model's window: 100M tokens, as WorkBuddy's own
// cards say ("100M tokens" a day).
const workbuddyLimit = 100_000_000

// workbuddyRows is every account and model the warm-up watches, in the
// order the providers are in, each account's models together.
func workbuddyRows(now time.Time) []workbuddyRow {
	warmed := provider.WorkBuddyWarmed()
	rows := make([]workbuddyRow, 0, len(warmed))
	for key, w := range warmed {
		_ = key // the key is the account and model, as the row has them
		row := workbuddyRow{
			Provider: w.Provider,
			User:     w.User,
			Model:    w.Model,
			Healthy:  w.Healthy,
			Status:   w.Status,
			Err:      w.Err,
			Attempts: w.Attempts,
			GaveUp:   w.GaveUp,
			Limit:    workbuddyLimit,
		}
		if !w.Until.IsZero() {
			until := w.Until
			row.Until = &until
			// the window it starts over ends is the 24 hours before it
			if !w.Healthy {
				start := until.Add(-provider.WorkBuddyWindow)
				row.StartsAt = &start
			}
		}
		if w.Healthy && !w.Until.IsZero() {
			// a window still answering began when the account did
			start := w.Until
			row.StartsAt = &start
		}
		rows = append(rows, row)
	}
	workbuddyFirstCalls(rows, now)
	workbuddySpent(rows, now)
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].User != rows[j].User {
			return rows[i].User < rows[j].User
		}
		return rows[i].Model < rows[j].Model
	})
	return rows
}

// workbuddyFirstCalls finds each row's earliest call in this window:
// after the thaw time if available/frozen, otherwise today's earliest call.
func workbuddyFirstCalls(rows []workbuddyRow, now time.Time) {
	y, m, d := now.Date()
	todayStart := time.Date(y, m, d, 0, 0, 0, 0, now.Location())

	earliest := make(map[string]time.Time, len(rows))
	floor := todayStart
	for _, r := range rows {
		key := wbRowKey(r.User, r.Model)
		if r.StartsAt != nil && r.StartsAt.Before(now) {
			earliest[key] = *r.StartsAt
			if r.StartsAt.Before(floor) {
				floor = *r.StartsAt
			}
		} else {
			earliest[key] = todayStart
		}
	}

	firstFound := make(map[string]time.Time, len(rows))
	for _, rec := range usage.Load(floor) {
		if rec.ProviderAccount == "" || rec.Time.Before(floor) {
			continue
		}
		key := wbRowKey(rec.ProviderAccount, rec.Model)
		bound, ok := earliest[key]
		if !ok || rec.Time.Before(bound) {
			continue
		}
		if cur, seen := firstFound[key]; !seen || rec.Time.Before(cur) {
			firstFound[key] = rec.Time
		}
	}

	for i := range rows {
		key := wbRowKey(rows[i].User, rows[i].Model)
		if t, ok := firstFound[key]; ok {
			rows[i].FirstCallAt = &t
		} else if bound, ok := earliest[key]; ok {
			rows[i].FirstCallAt = &bound
		}
	}
}

// workbuddySpent fills each row's Spent, Calls and Probed from the usage
// log: what magpie routed to that account and model inside the window the
// row names. A row naming no window is left at nothing, since what it
// spent is not known to be in any window.
func workbuddySpent(rows []workbuddyRow, now time.Time) {
	at := make(map[string]int, len(rows))
	window := make(map[string]time.Time, len(rows))
	for i, r := range rows {
		if r.StartsAt == nil || r.StartsAt.After(now) {
			continue
		}
		at[wbRowKey(r.User, r.Model)] = i
		window[wbRowKey(r.User, r.Model)] = *r.StartsAt
	}
	if len(at) == 0 {
		return
	}
	since := now
	for _, start := range window {
		if start.Before(since) {
			since = start
		}
	}
	for _, rec := range usage.Load(since) {
		if rec.Time.Before(since) || rec.ProviderAccount == "" {
			continue
		}
		key := wbRowKey(rec.ProviderAccount, rec.Model)
		i, ok := at[key]
		if !ok || rec.Time.Before(window[key]) {
			continue
		}
		tokens := int64(rec.Input + rec.Output + rec.CacheRead + rec.CacheWrite)
		// a probe is magpie's own call, and says nothing of what the
		// account's own use has been: counted apart, and left out of the
		// window's spending
		if rec.ByMagpie() {
			rows[i].Probed++
			continue
		}
		rows[i].Calls++
		rows[i].Spent += tokens
	}
}

// wbRowKey is an account and a model, as a row and a record name them.
func wbRowKey(user, model string) string { return user + "\x00" + model }
