// Package custom is the Custom page: WorkBuddy's free models watched on a
// schedule, what each probe answered, and the mode it runs in — a window
// used up is asked again once its reset has come, and an account that
// answers is asked every interval set here.
package custom

import (
	"github.com/yetone/magpie/internal/pluginrt"
	"github.com/yetone/magpie/internal/settings"
)

func init() {
	pluginrt.Register(pluginrt.Manifest{
		ID: "custom", API: "^1", Requires: []string{"window", "settings"},
		Name: "Custom", Sub: "WorkBuddy's free models on a schedule: what each probe answered, and how often to ask",
		One: "1 schedule on", Many: "{n} schedules on",
		InUse: func() (int, error) {
			s, err := settings.LoadChecked()
			if err != nil {
				return 0, err
			}
			if !s.WorkBuddyWatch.On {
				return 0, nil
			}
			return max(1, len(s.WorkBuddyWatch.Models)), nil
		},
		Parts:  []pluginrt.Part{{Slot: "nav.page", Key: "custom"}},
		Client: []string{"i18n.js", "custom.css", "page.js"},
	})
}
