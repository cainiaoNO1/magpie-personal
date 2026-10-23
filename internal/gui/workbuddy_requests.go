package gui

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/usage"
	"github.com/yetone/magpie/internal/usageapi"
)

// The Custom page's request view: one account and model's calls inside the
// window its card names, drawn as the Usage page's Requests tab draws
// them — the totals, the trend, the heatmap and the list — so what an
// account spent reads the same way here as there.
//
// The window is the row's: a window used up is the 24 hours before the
// reset WorkBuddy named, and one still answering began when its reset
// came. A row naming neither is read from the start of today.

// workbuddyWindowSpan is how long a free model's window is: WorkBuddy
// starts one over 24 hours after the first call that used it up.
const workbuddyWindowSpan = 24 * time.Hour

// workbuddyCalls is the view as the page is sent it.
type workbuddyCalls struct {
	// From and To are the window read; Why names how it was picked:
	// "window" for the row's own, "today" for one that has none.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	Why  string    `json:"why"`
	// Period is the rolling window those two make, for the ledger's own
	// queries (a CSV, the next page).
	Period usage.Period `json:"period"`
	// Probe and Use are how many of each the window holds, before the
	// page's pick narrows the list.
	Probe int `json:"probe"`
	Use   int `json:"use"`
	// Summary is the window's totals and its trend.
	Summary usage.Summary `json:"summary"`
	// Heat is the last weeks a day each, of this account's calls.
	Heat usage.Heatmap `json:"heat"`
	// Ledger is the window's requests, newest first, a page at a time.
	Ledger usageapi.Ledger `json:"ledger"`
}

// workbuddyCallsOf reads one account and model's calls: the window its
// card names, and the kind the page picked ("" both, "probe" the
// watching's own, "use" the account's).
func workbuddyCallsOf(user, model, kind string, offset, limit int, now time.Time) workbuddyCalls {
	from, why := workbuddyWindow(user, model, now)
	if from.After(now) {
		from = now.Add(-workbuddyWindowSpan)
	}
	// the window as the ledger reads one: a rolling span ending now, which
	// is what the row's own start and now are
	p, ok := usage.LastOf(now.Sub(from))
	if !ok {
		p, _ = usage.LastOf(workbuddyWindowSpan)
	}
	f := usage.Filter{Account: user, Model: model}
	out := workbuddyCalls{From: from, To: now, Why: why, Period: p}
	if kind == "probe" {
		f.Agent = "magpie"
	} else if kind == "use" {
		// the account's own calls: every agent but magpie's watching
		out.Summary = usage.Summarize(p)
		out.Ledger = usageapi.LedgerPage(p, f, offset, limit)
		workbuddyDropProbes(&out.Ledger)
	} else {
		out.Summary = usage.Summarize(p)
		out.Ledger = usageapi.LedgerPage(p, f, offset, limit)
	}
	out.Probe, out.Use = workbuddyCounts(p, user, model)
	out.Heat = usage.HeatmapOf(usage.Filter{Account: user, Model: model})
	return out
}

// workbuddyCounts is how many of the window's calls were the watching's
// own and how many the account's.
func workbuddyCounts(p usage.Period, user, model string) (probe, use int) {
	for _, r := range usage.Load(time.Time{}) {
		if r.ProviderAccount != user || (model != "" && r.Model != model) {
			continue
		}
		if !workbuddyIn(p, r.Time) {
			continue
		}
		if r.ByMagpie() {
			probe++
		} else {
			use++
		}
	}
	return probe, use
}

// workbuddyIn is whether t falls in the rolling window p names.
func workbuddyIn(p usage.Period, t time.Time) bool {
	d := p.Last()
	return d > 0 && !t.Before(usage.Clock().Add(-d))
}

// workbuddyDropProbes takes the watching's own calls out of a page read
// with every agent: the ledger has no "not magpie" filter.
func workbuddyDropProbes(l *usageapi.Ledger) {
	kept := l.Rows[:0]
	for _, r := range l.Rows {
		if !r.ByMagpie() {
			kept = append(kept, r)
		}
	}
	l.Rows = kept
}

// workbuddyOurs is whether a call went to that account and model; an
// empty model keeps every model of the account.
func workbuddyOurs(r usage.Record, user, model string) bool {
	if r.ProviderAccount != user {
		return false
	}
	return model == "" || r.Model == model
}

// workbuddyWindow is where an account and model's calls start:
// its first call time in this window (FirstCallAt), or startsAt if known,
// falling back to today's start.
func workbuddyWindow(user, model string, now time.Time) (time.Time, string) {
	y, m, d := now.Date()
	todayStart := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	for _, r := range workbuddyRows(now) {
		if r.User != user || (model != "" && r.Model != model) {
			continue
		}
		if r.FirstCallAt != nil && !r.FirstCallAt.After(now) {
			return *r.FirstCallAt, "window"
		}
		if r.StartsAt != nil && !r.StartsAt.After(now) {
			return *r.StartsAt, "window"
		}
		return todayStart, "today"
	}
	return todayStart, "today"
}

// workbuddyRequestRoutes is the Custom page's view of one account's calls.
func workbuddyRequestRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/workbuddy/requests", func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		user := strings.TrimSpace(q.Get("user"))
		if user == "" {
			http.Error(rw, "user is an account", http.StatusBadRequest)
			return
		}
		kind := q.Get("kind")
		if kind != "probe" && kind != "use" {
			kind = ""
		}
		offset, _ := strconv.Atoi(q.Get("offset"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 {
			limit = 100
		}
		writeJSON(rw, workbuddyCallsOf(user, strings.TrimSpace(q.Get("model")), kind, offset, limit, usage.Clock()))
	})
}
