package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// wbQuota is a candidate for one of WorkBuddy's accounts, as routing holds
// it while it decides.
func wbQuota(user string) candidate {
	return candidate{
		p:     provider.Provider{ID: "workbuddy-ai", Account: &provider.Account{Agent: "plugin:workbuddy-ai", User: user}},
		model: "deepseek-v4.1-flash",
		rest:  "workbuddy-ai@" + user + "/deepseek-v4.1-flash",
	}
}

// A refusal that names when the window starts over is recorded at that time:
// the account is watched from there, not from a guess.
func TestWorkBuddyQuotaRefusalKeptAtItsReset(t *testing.T) {
	fresh(t)
	provider.ResetWorkBuddyWarmed()
	t.Cleanup(provider.ResetWorkBuddyWarmed)
	body := []byte("WorkBuddy AI: usage exceeds frequency limit, but don't worry, your usage will reset at 2026-10-23 19:38:30 UTC+8, alternatively, you can switch to the other models to continue using it.")
	if why := failureOf(wbQuota("wb-reset@example.com"), http.StatusTooManyRequests, body); why != failQuota {
		t.Fatalf("why = %q, want %q", why, failQuota)
	}
	got := provider.WorkBuddyWarmed()
	if len(got) != 1 {
		t.Fatalf("watched %d accounts, want 1: %v", len(got), got)
	}
	for _, w := range got {
		want := time.Date(2026, 10, 23, 11, 38, 30, 0, time.UTC)
		if !w.Until.Equal(want) {
			t.Fatalf("until = %s, want %s", w.Until, want)
		}
	}
}

// A refusal that names no reset is still a window used up, and is recorded
// as the 24 hours WorkBuddy's free models start over in. Left unrecorded,
// such an account was never watched, and real requests went on finding it
// every quarter of an hour for a day.
func TestWorkBuddyQuotaRefusalWithoutAResetIsStillWatched(t *testing.T) {
	fresh(t)
	provider.ResetWorkBuddyWarmed()
	t.Cleanup(provider.ResetWorkBuddyWarmed)
	// a quota refusal, in words that name no time at all
	body := []byte(`{"error":{"message":"usage limit reached for this plan"}}`)
	if why := failureOf(wbQuota("wb-silent@example.com"), http.StatusTooManyRequests, body); why != failQuota {
		t.Fatalf("why = %q, want %q", why, failQuota)
	}
	got := provider.WorkBuddyWarmed()
	if len(got) != 1 {
		t.Fatalf("watched %d accounts, want 1: %v", len(got), got)
	}
	for _, w := range got {
		left := time.Until(w.Until)
		if left < provider.WorkBuddyWindow-time.Minute || left > provider.WorkBuddyWindow+time.Minute {
			t.Fatalf("rests %s, want about %s", left, provider.WorkBuddyWindow)
		}
	}
}

// A 429 that is only a rate limit is not a window used up, and is left to
// the rate rest: watching it as a day's quota would hold an account that
// answers again in a minute out for a day.
func TestWorkBuddyRateLimitIsNotAWindow(t *testing.T) {
	fresh(t)
	provider.ResetWorkBuddyWarmed()
	t.Cleanup(provider.ResetWorkBuddyWarmed)
	if why := failureOf(wbQuota("wb-rate@example.com"), http.StatusTooManyRequests, []byte("WorkBuddy AI: too many requests")); why != failRate {
		t.Fatalf("why = %q, want %q", why, failRate)
	}
	if got := provider.WorkBuddyWarmed(); len(got) != 0 {
		t.Fatalf("watched %v, want nothing", got)
	}
}
