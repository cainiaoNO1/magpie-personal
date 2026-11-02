// Package gemini is the Gemini page: Antigravity's rolling 5-hour and
// 7-day windows, opened on a schedule so the clock on the 5-hour one
// starts even when nothing else calls the account.
//
// A window's hours run from the first call that draws on it, not from the
// reset before it: an account nobody calls never starts its clock, and
// the window it was given goes unused. The watching reads the window's
// own usage upstream, makes that one call when nothing has drawn on it,
// and looks again after a random wait inside the interval set on the page.
package gemini

import (
	"github.com/yetone/magpie/internal/pluginrt"
	"github.com/yetone/magpie/internal/settings"
)

func init() {
	pluginrt.Register(pluginrt.Manifest{
		ID: "gemini", API: "^1", Requires: []string{"window", "settings"},
		Name: "Gemini", Sub: "Antigravity's 5-hour window, opened on a schedule so its clock starts",
		One: "1 account opened on a schedule", Many: "{n} accounts opened on a schedule",
		InUse: func() (int, error) {
			s, err := settings.LoadChecked()
			if err != nil {
				return 0, err
			}
			if !s.GeminiWatch.On {
				return 0, nil
			}
			return 1, nil
		},
		Parts:  []pluginrt.Part{{Slot: "nav.page", Key: "gemini"}},
		Client: []string{"i18n.js", "custom.css", "page.js"},
	})
}
