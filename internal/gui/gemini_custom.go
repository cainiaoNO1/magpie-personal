package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
)

// The Gemini page: Antigravity's pools opened on a schedule.
//
// A window's hours start at the first call that draws on it, not at the
// reset before it: an account nobody calls never starts its clock, and the
// window it was given goes unused. Antigravity bills an account's models
// against pools of their own ("Gemini", "Claude & GPT", and on some plans
// "Gemini 4 Argon"), so the watching reads every pool's own usage upstream
// and makes that one call for each pool whose governing window has nothing
// drawn on it — the 5-hour one where the pool has one, else the 7-day one,
// which is the only window the Argon pool reports. It looks again after a
// random wait inside the interval set here.
//
// The page reads and writes the mode and the interval, and lists every
// Antigravity account with the pools the watching last read.

// geminiCustom is the page's state: the mode, and the rows it draws.
type geminiCustom struct {
	On bool `json:"on"`
	// Every is the longest the watching waits between looks, in seconds;
	// zero is magpie's own. The wait itself is random, inside the last
	// fifth of it, so a schedule never lands on the same second twice.
	Every int `json:"every"`
	// Default is magpie's own interval, in seconds, for the page to say
	// what zero means.
	Default int `json:"default"`
	// Min is the shortest interval the watching may be set to.
	Min int `json:"min"`
	// Rows are the Antigravity accounts watched now, each with its pools.
	Rows []provider.GeminiRow `json:"rows"`
}

func geminiCustomState(now time.Time) geminiCustom {
	s := settings.Load()
	return geminiCustom{
		On:      s.GeminiWatch.On,
		Every:   s.GeminiWatch.Every,
		Default: int(provider.GeminiWatchDefault() / time.Second),
		Min:     int(provider.GeminiWatchMin() / time.Second),
		Rows:    geminiRows(now),
	}
}

// geminiRows is the accounts watched, never nil: the page draws its empty
// state from a list, and a JSON null would read as a page that failed.
func geminiRows(now time.Time) []provider.GeminiRow {
	if rows := provider.GeminiRows(now); rows != nil {
		return rows
	}
	return []provider.GeminiRow{}
}

func geminiCustomRoutes(mux *http.ServeMux) {
	// the page's state: the mode, the interval, and every account's pools.
	// asked=1 is the page's Refresh: the allowances are read now rather than
	// shown as the last look left them, which is minutes old and can be from
	// before a window reset. The read is bounded, so a slow vendor answers
	// the page with the last figures instead of holding the request.
	mux.HandleFunc("GET /api/gemini/custom", func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("asked") != "" {
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			provider.RefreshGemini(ctx, usage.Clock())
			cancel()
		}
		writeJSON(rw, geminiCustomState(usage.Clock()))
	})
	// the mode and the interval, as the page sets them
	mux.HandleFunc("POST /api/gemini/custom", func(rw http.ResponseWriter, r *http.Request) {
		var in struct {
			On    bool `json:"on"`
			Every int  `json:"every"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			fail(rw, err)
			return
		}
		min := int(provider.GeminiWatchMin() / time.Second)
		if in.Every != 0 && in.Every < min {
			http.Error(rw, "every is at least "+strconv.Itoa(min)+" seconds", http.StatusBadRequest)
			return
		}
		s := settings.Load()
		s.GeminiWatch = settings.GeminiWatch{On: in.On, Every: in.Every}
		if err := settings.Save(s); err != nil {
			fail(rw, err)
			return
		}
		applyGeminiWatch(s.GeminiWatch)
		writeJSON(rw, geminiCustomState(usage.Clock()))
	})
}

// applyGeminiWatch hands the setting to the watching, which starts its
// round over: what the page saves is picked up without a restart.
func applyGeminiWatch(w settings.GeminiWatch) {
	provider.SetGeminiWatch(provider.GeminiWatchOf(w.On, w.EveryFor()))
}
