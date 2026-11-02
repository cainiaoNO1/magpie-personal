package settings

import "time"

// GeminiWatch is the Gemini page's self-use warm-up: whether magpie keeps
// an Antigravity account's 5-hour window started, and how often it looks.
//
// The window runs 5 hours from the first call that spends anything of it,
// and one that is never called never begins. Looking on a schedule and
// making that first call while nothing is used is what keeps the window a
// day's use sits in aligned with the user's own requests rather than with
// the first one after a night's rest. The 7-day window is shown beside it
// and isn't watched: its reset starts the 5-hour one too.
type GeminiWatch struct {
	// On is whether the warm-up looks at all.
	On bool `json:"on,omitempty"`
	// Every is the longest wait between two looks, in seconds: each look
	// waits a random while within [0.8, 1] of it, so a first call doesn't
	// land on the same clock time twice running. Zero is magpie's own five
	// minutes.
	Every int `json:"every,omitempty"`
}

// EveryFor is Every as a duration.
func (w GeminiWatch) EveryFor() time.Duration { return time.Duration(w.Every) * time.Second }
