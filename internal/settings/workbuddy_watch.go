package settings

import "time"

// WorkBuddyWatch is the Custom page's scheduled requests for WorkBuddy's
// free models: which models are watched, and how often an account that
// answers is asked.
//
// A window used up is asked again on the warm-up's own schedule whatever
// this says: the interval here is the watching of an account that answers,
// and the return of one that ran out is the 24 hours the vendor named.
type WorkBuddyWatch struct {
	// On is whether the scheduled requests run at all.
	On bool `json:"on,omitempty"`
	// Models are the models watched on every account.
	Models []string `json:"models,omitempty"`
	// Every is how often a healthy account and model is asked, in
	// seconds; zero is magpie's own half hour.
	Every int `json:"every,omitempty"`
	// Per is an account and model's own interval, over Every: the key is
	// "user\x00model", as the watching keeps them.
	Per map[string]int `json:"per,omitempty"`
}

// EveryFor is Every as a duration.
func (w WorkBuddyWatch) EveryFor() time.Duration { return time.Duration(w.Every) * time.Second }

// PerFor is one account and model's own interval, zero when it has none.
func (w WorkBuddyWatch) PerFor(user, model string) time.Duration {
	return time.Duration(w.Per[user+"\x00"+model]) * time.Second
}
