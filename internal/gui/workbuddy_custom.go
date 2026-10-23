package gui

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
)

// The Custom page: WorkBuddy's free models watched on a schedule, what the
// watching has asked and what came back, and the mode it runs in.
//
// Two modes, as the reader asked for them (workbuddy_warmup.go):
//   - a window used up is asked again once its reset has come, and counted
//     back in only on a 200, retried at a random wait until then;
//   - an account that answers is asked every interval the reader sets.
//
// The page reads and writes both, and lists every probe.

// workbuddyCustom is the page's state: the mode, and the rows it draws.
type workbuddyCustom struct {
	On bool `json:"on"`
	// Models are the models watched on every account.
	Models []string `json:"models"`
	// Every is how often a healthy account is asked, in seconds; zero is
	// magpie's own half hour.
	Every int `json:"every"`
	// Per is an account and model's own interval, by "user\x00model".
	Per map[string]int `json:"per,omitempty"`
	// Served are the models the WorkBuddy plugin lists, for the page's
	// choice of what to watch.
	Served []string `json:"served"`
	// Default is magpie's own interval, in seconds, for the page to say
	// what zero means.
	Default int `json:"default"`
	// Min is the shortest interval a healthy account may be asked at.
	Min int `json:"min"`
	// Rows are the accounts and models watched now.
	Rows []workbuddyRow `json:"rows"`
}

func workbuddyCustomState(now time.Time) workbuddyCustom {
	s := settings.Load()
	out := workbuddyCustom{
		On:      s.WorkBuddyWatch.On,
		Models:  append([]string(nil), s.WorkBuddyWatch.Models...),
		Every:   s.WorkBuddyWatch.Every,
		Per:     s.WorkBuddyWatch.Per,
		Default: int(provider.WorkBuddyWatchDefault() / time.Second),
		Min:     int(provider.WorkBuddyWatchMin() / time.Second),
	}
	if out.Models == nil {
		out.Models = []string{}
	}
	out.Served = workbuddyServedModels()
	out.Rows = workbuddyRows(now)
	return out
}

// workbuddyServedModels is the free models the WorkBuddy plugin lists, for
// the page to offer: only these run against the window that is watched,
// and the rest are the plan's paid ones.
func workbuddyServedModels() []string {
	pp, ok := provider.PluginOf(provider.WorkBuddyAIID)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(pp.Models))
	for _, m := range pp.Models {
		if m.Free {
			out = append(out, m.ID)
		}
	}
	return out
}

func workbuddyCustomRoutes(mux *http.ServeMux) {
	// the page's state: the mode, the models watched, and every row
	mux.HandleFunc("GET /api/workbuddy/custom", func(rw http.ResponseWriter, r *http.Request) {
		writeJSON(rw, workbuddyCustomState(usage.Clock()))
	})
	// the mode and the intervals, as the page sets them
	mux.HandleFunc("POST /api/workbuddy/custom", func(rw http.ResponseWriter, r *http.Request) {
		var in struct {
			On     bool           `json:"on"`
			Models []string       `json:"models"`
			Every  int            `json:"every"`
			Per    map[string]int `json:"per"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			fail(rw, err)
			return
		}
		min := int(provider.WorkBuddyWatchMin() / time.Second)
		if in.Every != 0 && in.Every < min {
			http.Error(rw, "every is at least "+strconv.Itoa(min)+" seconds", http.StatusBadRequest)
			return
		}
		per := map[string]int{}
		for k, v := range in.Per {
			if v >= min {
				per[k] = v
			}
		}
		s := settings.Load()
		s.WorkBuddyWatch = settings.WorkBuddyWatch{On: in.On, Models: in.Models, Every: in.Every, Per: per}
		if err := settings.Save(s); err != nil {
			fail(rw, err)
			return
		}
		applyWorkBuddyWatch(s.WorkBuddyWatch)
		writeJSON(rw, workbuddyCustomState(usage.Clock()))
	})
	// every probe kept, newest first, the account's own when asked
	mux.HandleFunc("GET /api/workbuddy/probes", func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 || limit > 500 {
			limit = 200
		}
		writeJSON(rw, provider.WorkBuddyProbes(strings.TrimSpace(q.Get("user")), q.Get("model"), limit))
	})
}

// applyWorkBuddyWatch hands the setting to the watching: the models asked
// on every account, and how often an account that answers is asked.
func applyWorkBuddyWatch(w settings.WorkBuddyWatch) {
	provider.SetWorkBuddyWatch(provider.WorkBuddyWatchOf(w.On, w.Models, w.EveryFor(), w.Per))
}
