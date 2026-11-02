package provider

// The Gemini warm-up (gemini_warmup.go): which pools it reads, which window
// of a pool says whether it is open, which model it calls for each pool, and
// what one look does with a pool that is unused, already used, or couldn't be
// read.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/settings"
)

// gemAt is a fixed instant, so a test says the times it means.
func gemAt(min int) time.Time {
	return time.Date(2026, 11, 5, 9, min, 0, 0, time.UTC)
}

// gemHome keeps a test off the machine's own settings, providers and
// catalogues: the watching keeps its state beside providers.json, and the
// model it calls comes from a list kept in the vendor's cache.
func gemHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	catalog.Reset()
	t.Cleanup(catalog.Reset)
}

// gemReset forgets what an earlier look kept, and has the watching on
// unless a test says otherwise.
func gemReset(t *testing.T, on bool, every time.Duration) {
	t.Helper()
	gemWarmMu.Lock()
	gemWarmMu.state, gemWarmMu.read, gemWarmMu.scanned = map[string]gemWarm{}, nil, time.Time{}
	gemWarmMu.Unlock()
	SetGeminiWatch(GeminiWatchOf(on, every))
	t.Cleanup(func() {
		gemWarmMu.Lock()
		gemWarmMu.state, gemWarmMu.read, gemWarmMu.scanned = nil, nil, time.Time{}
		gemWarmMu.Unlock()
		SetGeminiWatch(GeminiWatch{})
		SetGeminiProbeURL("")
	})
}

// gemBucket is one of a pool's windows as pools (google.go) makes it: Aside,
// no model of its own, the pool's name as its Pool. A bucket the summary gave
// no remainingFraction for keeps Used 100 and has no reset beside it.
func gemBucket(pool, name string, span time.Duration, used float64, reset *time.Time) QuotaWindow {
	w := QuotaWindow{Pool: pool, Name: name, Span: span, Used: used, Aside: true}
	if reset != nil {
		w.ResetsAt = reset
		w.ResetSecs = 3600
	}
	return w
}

// gemWindowsOf is one pool's buckets, its 5 hours and its 7 days, either left
// out where the pool has none: measured, "Gemini 4 Argon" reports a weekly
// bucket and no 5-hour one at all.
func gemWindowsOf(pool string, five, week float64, reset *time.Time) []QuotaWindow {
	var out []QuotaWindow
	if five >= 0 {
		out = append(out, gemBucket(pool, "5 hours", 5*time.Hour, five, reset))
	}
	if week >= 0 {
		out = append(out, gemBucket(pool, "7 days", 7*24*time.Hour, week, reset))
	}
	return out
}

// gemPoolReading is one pool as a test's reading has it: what has been drawn
// on each of its windows, and whether it has a 5-hour one at all.
type gemPoolReading struct {
	pool   string
	five   float64
	week   float64
	noFive bool
}

// gemAllowance is one Antigravity account's allowance as a look reads it: the
// pools the test names, each with the windows it has, and the per-model
// windows fetchAvailableModels gives beside them.
func gemAllowance(user string, reset *time.Time, pools ...gemPoolReading) SubscriptionQuota {
	ws := []QuotaWindow{
		{Name: "Gemini 3.7 Flash (High)", Model: "gemini-3.7-flash-high", Family: "Gemini", Pool: gemPool},
		{Name: "Claude Opus 4.6 (Thinking)", Model: "claude-opus-4-6-thinking", Family: "Claude", Pool: gemClaudePool, Used: 40},
	}
	for _, p := range pools {
		five := p.five
		if p.noFive {
			five = -1
		}
		ws = append(ws, gemWindowsOf(p.pool, five, p.week, reset)...)
	}
	return SubscriptionQuota{Provider: "antigravity", Name: "Antigravity", User: user, Windows: ws}
}

// gemTwoPools is the allowance of the accounts that have two pools, nothing
// drawn on either: what a look sees before it opens them.
func gemTwoPools(user string, reset *time.Time) SubscriptionQuota {
	return gemAllowance(user, reset,
		gemPoolReading{pool: gemPool},
		gemPoolReading{pool: gemClaudePool, week: 62})
}

// gemThreePools is the pix account's, measured: Argon carries a weekly window
// and no 5-hour one, and nothing has drawn on any of the three.
func gemThreePools(user string, reset *time.Time) SubscriptionQuota {
	return gemAllowance(user, reset,
		gemPoolReading{pool: gemPool},
		gemPoolReading{pool: gemClaudePool, week: 62},
		gemPoolReading{pool: gemArgonPool, noFive: true})
}

// gemWithoutModels is q without the per-model windows whose id has word in
// it: an account whose plan lists no model of that family.
func gemWithoutModels(q SubscriptionQuota, word string) SubscriptionQuota {
	out := q
	out.Windows = nil
	for _, w := range q.Windows {
		if w.Model != "" && strings.Contains(w.Model, word) {
			continue
		}
		out.Windows = append(out.Windows, w)
	}
	return out
}

// gemStateOf is what a look kept of one account.
func gemStateOf(t *testing.T, user string) gemWarm {
	t.Helper()
	gemWarmMu.Lock()
	defer gemWarmMu.Unlock()
	return gemWarmMu.state[strings.ToLower(user)]
}

// gemPoolState is what a look kept of one pool of one account.
func gemPoolState(t *testing.T, user, pool string) gemPoolWarm {
	t.Helper()
	return gemStateOf(t, user).poolState(pool)
}

// gemCall is one request the fake gateway was sent.
type gemCall struct{ account, model, ua, path, body string }

// gemGateway is a stand-in for the gateway a first call goes through: it
// records each request and answers 200 unless status says otherwise. What it
// recorded is read behind its lock: a fake server's handler runs on many
// goroutines.
func gemGateway(t *testing.T, status int, said string) (*httptest.Server, func() []gemCall) {
	t.Helper()
	var mu sync.Mutex
	var got []gemCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var sent struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(b, &sent)
		mu.Lock()
		got = append(got, gemCall{account: r.Header.Get("X-Magpie-Account"), model: sent.Model,
			ua: r.Header.Get("User-Agent"), path: r.URL.Path, body: string(b)})
		mu.Unlock()
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			io.WriteString(w, said)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []gemCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]gemCall(nil), got...)
	}
}

// gemSaveModels is one account's own model list, as its plan lists it.
func gemSaveModels(t *testing.T, user string, ms ...catalog.Model) {
	t.Helper()
	if err := catalog.SaveLive(accountModels("antigravity", user), "", ms); err != nil {
		t.Fatal(err)
	}
}

// gemList is a list of ids as Antigravity answers them, none with a price
// models.dev knows.
func gemList(ids ...string) []catalog.Model {
	out := make([]catalog.Model, 0, len(ids))
	for _, id := range ids {
		out = append(out, catalog.Model{ID: id})
	}
	return out
}

// The page may not set an interval shorter than the vendor's quota endpoint
// is worth asking, and zero is magpie's own five minutes.
func TestGeminiWatchIntervalClamped(t *testing.T) {
	gemHome(t)
	gemReset(t, false, 0)
	for _, c := range []struct{ set, want time.Duration }{
		{0, 0},
		{30 * time.Second, 0},
		{time.Minute, time.Minute},
		{20 * time.Minute, 20 * time.Minute},
	} {
		if got := GeminiWatchOf(true, c.set); got.Every != c.want || !got.On {
			t.Errorf("GeminiWatchOf(true, %v) = %+v, want every %v", c.set, got, c.want)
		}
	}
	if GeminiWatchMin() != time.Minute || GeminiWatchDefault() != 5*time.Minute {
		t.Errorf("min %v, default %v; want a minute and five", GeminiWatchMin(), GeminiWatchDefault())
	}
}

// A look waits somewhere in the last fifth of the interval set, and a day's
// looks don't land on one time: the interval is an upper bound, not the
// wait.
func TestGeminiWaitInsideTheLastFifth(t *testing.T) {
	gemHome(t)
	gemReset(t, true, 10*time.Minute)
	seen := map[time.Duration]bool{}
	for i := 0; i < 2000; i++ {
		if w := gemWait(); w < 8*time.Minute || w > 10*time.Minute {
			t.Fatalf("a wait of %v is outside [8m, 10m]", w)
		}
		seen[gemWait()] = true
	}
	if len(seen) < 2 {
		t.Fatal("every wait is the same: nothing is randomized")
	}
	SetGeminiWatch(GeminiWatch{On: true}) // no interval set: magpie's own
	for i := 0; i < 200; i++ {
		if w := gemWait(); w < 4*time.Minute || w > 5*time.Minute {
			t.Fatalf("with no interval set a wait of %v is outside [4m, 5m]", w)
		}
	}
}

// Every pool an account reports is read as a pool of its own, with the
// windows that pool has: the two every plan carries, and the third the pix
// account carries, measured with a weekly window and no 5-hour one. The
// pools' figures are never added: one pool is one pool.
func TestGeminiPoolsAreEachPoolOfTheAccount(t *testing.T) {
	reset := gemAt(30)
	for _, c := range []struct {
		user  string
		argon bool
		want  map[string]int // pool -> how many windows it has
	}{
		// the measured plans
		{"y15570087076@gmail.com", false, map[string]int{gemPool: 2, gemClaudePool: 2}},
		{"y553992077@gmail.com", false, map[string]int{gemPool: 2, gemClaudePool: 2}},
		{"y19913868365@gmail.com", true, map[string]int{gemPool: 2, gemClaudePool: 2, gemArgonPool: 1}},
	} {
		q := gemTwoPools(c.user, &reset)
		if c.argon {
			q = gemThreePools(c.user, &reset)
		}
		pools := gemPools(q)
		if len(pools) != len(c.want) {
			t.Fatalf("%s: %d pools %+v, want %d", c.user, len(pools), pools, len(c.want))
		}
		for _, p := range pools {
			if n, ok := c.want[p.Pool]; !ok || len(p.windows()) != n {
				t.Errorf("%s: pool %q has %+v, want %d windows", c.user, p.Pool, p.windows(), n)
			}
		}
		// a pool's figure is its own: the Gemini pool's 5 hours is not the
		// Claude & GPT one's
		for _, p := range pools {
			switch p.Pool {
			case gemPool:
				if p.Five == nil || p.Five.Used != 0 {
					t.Errorf("%s: the Gemini pool's 5 hours is %+v", c.user, p.Five)
				}
			case gemClaudePool:
				if p.Five == nil || p.Five.Used != 0 || p.Week == nil || p.Week.Used != 62 {
					t.Errorf("%s: the Claude & GPT pool is %+v", c.user, p)
				}
			case gemArgonPool:
				if p.Five != nil || p.Week == nil || p.Week.Used != 0 || p.Week.Name != "7 days" {
					t.Errorf("%s: the Argon pool is %+v", c.user, p)
				}
			}
		}
	}
}

// A pool is read by its governing window: its 5 hours where it has one, else
// its 7 days. Measured, "Gemini 4 Argon" has no 5-hour bucket at all, so the
// weekly one is what says whether its clock is running — and a pool whose
// 5-hour window has reset to nothing is not open because its 7-day one still
// carries use.
func TestGeminiGoverningWindowOfAPool(t *testing.T) {
	reset := gemAt(30)
	pools := gemPools(gemAllowance("a@example.com", &reset,
		gemPoolReading{pool: gemPool, five: 0, week: 91},
		gemPoolReading{pool: gemClaudePool, five: 0.0015, week: 0.0008},
		gemPoolReading{pool: gemArgonPool, noFive: true, week: 23.45}))
	if len(pools) != 3 {
		t.Fatalf("the pools are %+v, want three", pools)
	}
	byName := map[string]gemPoolWindows{}
	for _, p := range pools {
		byName[p.Pool] = p
	}
	if gov := byName[gemPool].governing(); gov == nil || gov.Name != "5 hours" || gov.Used != 0 {
		t.Errorf("the Gemini pool is read by %+v, want its 5 hours at 0", gov)
	}
	if gov := byName[gemClaudePool].governing(); gov == nil || gov.Name != "5 hours" || gov.Used != 0.0015 {
		t.Errorf("the Claude & GPT pool is read by %+v, want its 5 hours", gov)
	}
	// the weekly-only pool: its 7 days is the one window it has, and the one
	// that says whether it is open
	argon := byName[gemArgonPool]
	if argon.Five != nil {
		t.Fatalf("the Argon pool has a 5-hour window: %+v", argon.Five)
	}
	if gov := argon.governing(); gov == nil || gov.Name != "7 days" || gov.Used != 23.45 {
		t.Errorf("the Argon pool is read by %+v, want its 7 days", gov)
	}
	if got := argon.windows(); len(got) != 1 || got[0].Name != "7 days" {
		t.Errorf("the Argon pool is shown %+v, want its 7 days alone", got)
	}
}

// Without the summary of groups a group's 5-hour figure is on each of its
// models, the tightest of them being the group's own.
func TestGeminiPoolsFallBackToModels(t *testing.T) {
	q := SubscriptionQuota{Provider: "antigravity", User: "a@example.com", Windows: []QuotaWindow{
		{Name: "Gemini 3.7 Flash (High)", Model: "gemini-3.7-flash-high", Family: "Gemini", Used: 30},
		{Name: "Gemini 3.5 Flash Lite", Model: "gemini-3.5-flash-lite", Family: "Gemini", Used: 55},
		{Name: "Claude Opus 4.6 (Thinking)", Model: "claude-opus-4-6-thinking", Family: "Claude", Used: 90},
	}}
	pools := gemPools(q)
	if len(pools) != 1 || pools[0].Pool != gemPool {
		t.Fatalf("the pools are %+v, want the Gemini one alone", pools)
	}
	if gov := pools[0].governing(); gov == nil || gov.Used != 55 || pools[0].Week != nil {
		t.Errorf("the Gemini pool is %+v, want the tightest Gemini model and no week", pools[0])
	}
}

// A bucket the summary gave no figure for is not a window used up: it is
// left out of what the page draws rather than shown at the 100 pools starts
// it with.
func TestGeminiWindowsLeaveOutAWindowWithNoFigure(t *testing.T) {
	week := gemAt(30)
	q := SubscriptionQuota{Provider: "antigravity", User: "a@example.com", Windows: []QuotaWindow{
		gemBucket(gemPool, "5 hours", 5*time.Hour, 100, nil),
		gemBucket(gemPool, "7 days", 7*24*time.Hour, 74, &week),
	}}
	pools := gemPools(q)
	if len(pools) != 1 {
		t.Fatalf("the pools are %+v, want one", pools)
	}
	if gemRead(*pools[0].Five) {
		t.Error("a bucket with no reset beside it was read as a figure of the vendor's")
	}
	if !gemRead(*pools[0].Week) {
		t.Error("a bucket with a reset beside it was not read as a figure")
	}
	if got := pools[0].windows(); len(got) != 1 || got[0].Name != "7 days" {
		t.Errorf("the page is shown %+v, want the 7 days alone", got)
	}
}

// The Gemini pool's first call asks the family that draws on it, not the
// cheapest model: gemini-3.8-flash where the account lists it, else the
// cheapest flash-lite, else of the flashes. A plan with none of the 3.8
// family still gets a call. Antigravity's per-level ids are asked for as the
// family magpie offers.
//
// Measured on a live account, this is the whole bug: 2157 tokens sent to
// gemini-2.5-flash-lite moved its 5-hour figure from 0.0164 to 0.0164, and
// the watching then sent another first call every look — 176 of them. A
// gemini-3.8-flash call moved it 0.0000 -> 0.0164, and left the Argon pool's
// 7-day figure at 0.2345.
func TestGeminiModelPicksTheFamilyThatDrawsOnThePool(t *testing.T) {
	gemHome(t)
	if err := os.MkdirAll(filepath.Dir(catalog.CachePath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalog.CachePath(), []byte(`{"google":{"models":{
		"gemini-3.1-flash-lite":{"id":"gemini-3.1-flash-lite","cost":{"input":0.1,"output":0.4}},
		"gemini-3.5-flash-lite":{"id":"gemini-3.5-flash-lite","cost":{"input":0.3,"output":2.5}},
		"gemini-3.7-flash":{"id":"gemini-3.7-flash","cost":{"input":0.5,"output":3}}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog.Reset()
	for _, c := range []struct {
		name   string
		listed []catalog.Model
		want   string
	}{
		// the live account's own list, as Antigravity answered it: 3.8-flash
		// is one of eleven, and the only one that starts the clock
		{"the account's own eleven models", []catalog.Model{
			{ID: "gemini-2.5-flash"}, {ID: "gemini-2.5-flash-lite"}, {ID: "gemini-3-flash"}, {ID: "gemini-3-flash-agent"},
			{ID: "gemini-3.1-flash-image"}, {ID: "gemini-3.1-flash-lite"}, {ID: "gemini-3.5-flash"}, {ID: "gemini-3.5-flash-lite"},
			{ID: "gemini-3.8-flash"}, {ID: "gemini-3.8-flash-tiered"}, {ID: "gemini-4-argon-disabled"},
		}, "gemini-3.8-flash"},
		{"3.8-flash, however cheap the lite is", []catalog.Model{
			{ID: "gemini-2.5-flash-lite"}, {ID: "gemini-3.8-flash"},
		}, "gemini-3.8-flash"},
		// a plan that lists only the levels: the family is what is asked for,
		// since -low and -medium answer with no usage at all
		{"3.8-flash by its levels", []catalog.Model{
			{ID: "gemini-3.8-flash-low", Name: "Gemini 3.8 Flash (Low)"},
			{ID: "gemini-3.8-flash-medium", Name: "Gemini 3.8 Flash (Medium)"},
			{ID: "gemini-3.8-flash-high", Name: "Gemini 3.8 Flash (High)"},
			{ID: "gemini-2.5-flash-lite"},
		}, "gemini-3.8-flash"},
		{"the tiered id where that is all there is", []catalog.Model{
			{ID: "gemini-2.5-flash-lite"}, {ID: "gemini-3.8-flash-tiered"},
		}, "gemini-3.8-flash-tiered"},
		// none of the 3.8 family: a call anyway, so an older plan's window
		// still starts where its own cheapest model happens to start one
		{"2.5-flash-lite where the plan has no 3.8", []catalog.Model{
			{ID: "gemini-3.7-flash-low"}, {ID: "gemini-3.1-flash-lite"}, {ID: "gemini-2.5-flash-lite"}, {ID: "gemini-3.7-flash-high"},
		}, "gemini-2.5-flash-lite"},
		{"the cheapest flash-lite with no 3.8", []catalog.Model{
			{ID: "gemini-3.5-flash-lite"}, {ID: "gemini-3.7-flash-low"}, {ID: "gemini-3.1-flash-lite"},
		}, "gemini-3.1-flash-lite"},
		{"a flash when there is no lite", []catalog.Model{
			{ID: "gemini-3.7-flash-low"}, {ID: "gemini-3.7-flash-high"},
		}, "gemini-3.7-flash"},
		{"nothing Gemini", []catalog.Model{{ID: "claude-opus-4-6-thinking"}, {ID: "gpt-oss-120b-medium"}}, ""},
		{"no model at all", nil, ""},
	} {
		if err := catalog.SaveLive("antigravity", "", c.listed); err != nil {
			t.Fatal(err)
		}
		if got := gemModelFor("a@example.com", gemPool, SubscriptionQuota{}); got != c.want {
			t.Errorf("%s: gemModelFor = %q, want %q", c.name, got, c.want)
		}
	}
}

// Each pool is opened with a model of its own, taken from the account's own
// list: a sonnet for Claude & GPT where the plan lists one, and the Argon
// family itself for Argon. The pix account is the case that makes the list
// the only source — it refuses claude-sonnet-4-6 ("X-Magpie-Account:
// y19913868365@gmail.com's plan doesn't list claude-sonnet-4-6", measured)
// and lists claude-sonnet-5-5 instead.
func TestGeminiModelForEachPoolFromTheAccountsList(t *testing.T) {
	gemHome(t)
	const user = "y19913868365@gmail.com"
	for _, c := range []struct {
		name   string
		listed []catalog.Model
		pool   string
		want   string
	}{
		// the pix account's own list, measured: no claude-sonnet-4-6 on it
		{"a sonnet, the one this plan lists", gemList("claude-opus-4-6", "claude-sonnet-5-5", "gemini-3.8-flash"), gemClaudePool, "claude-sonnet-5-5"},
		// two of them, priced: the cheaper one is asked for, whatever order
		// the plan lists them in
		{"the cheapest sonnet where it lists two", []catalog.Model{
			{ID: "claude-sonnet-5-5", Price: &catalog.Price{Input: 3, Output: 15}},
			{ID: "claude-sonnet-4-5", Price: &catalog.Price{Input: 1, Output: 5}},
		}, gemClaudePool, "claude-sonnet-4-5"},
		{"any claude where there is no sonnet", gemList("claude-opus-4-6", "gpt-oss-120b"), gemClaudePool, "claude-opus-4-6"},
		{"a gpt-oss where there is no claude", gemList("gpt-oss-120b", "gemini-3.8-flash"), gemClaudePool, "gpt-oss-120b"},
		{"nothing of that pool", gemList("gemini-3.8-flash"), gemClaudePool, ""},
		// the Argon pool: the family itself, never the -disabled id the
		// account carries beside it
		{"the Argon family", gemList("gemini-4-argon", "gemini-4-argon-disabled"), gemArgonPool, "gemini-4-argon"},
		{"a variant where the family is not listed", gemList("gemini-4-argon-disabled", "gemini-4-argon-trial"), gemArgonPool, "gemini-4-argon-trial"},
		{"only the disabled id", gemList("gemini-4-argon-disabled"), gemArgonPool, ""},
		// a pool magpie knows no model of
		{"a pool nobody knows", gemList("gemini-3.8-flash", "claude-sonnet-5-5"), "Gemini 5 Lyra", ""},
	} {
		gemSaveModels(t, user, c.listed...)
		if got := gemModelFor(user, c.pool, SubscriptionQuota{}); got != c.want {
			t.Errorf("%s: gemModelFor(%q) = %q, want %q", c.name, c.pool, got, c.want)
		}
	}
}

// The pix account's measured shape: three pools, Argon with a weekly window
// alone, and a model list that names no claude-sonnet-4-6. Each pool is given
// exactly one first call of its own, with a model of that pool from that
// list; a pool already drawing on its governing window is given none.
func TestGeminiWarmOpensEveryPoolOnce(t *testing.T) {
	gemHome(t)
	gemReset(t, true, 5*time.Minute)
	const user = "y19913868365@gmail.com"
	gemSaveModels(t, user, gemList("gemini-2.5-flash-lite", "gemini-3.8-flash", "gemini-4-argon",
		"gemini-4-argon-disabled", "claude-sonnet-5-5", "claude-opus-4-6", "gpt-oss-120b")...)
	srv, calls := gemGateway(t, 0, "")
	SetGeminiProbeURL(srv.URL + "/")

	reset := gemAt(30)
	read := gemThreePools(user, &reset)
	LoginUsageVia(func(context.Context, string) map[string]SubscriptionQuota {
		return map[string]SubscriptionQuota{user: read}
	})
	t.Cleanup(func() { LoginUsageVia(nil) })

	gemWarmNow(context.Background(), gemAt(0))
	want := map[string]string{gemClaudePool: "claude-sonnet-5-5", gemPool: "gemini-3.8-flash", gemArgonPool: "gemini-4-argon"}
	if len(calls()) != len(want) {
		t.Fatalf("the look sent %d calls %+v, want one for each of the three pools", len(calls()), calls())
	}
	asked := map[string]bool{}
	for _, c := range calls() {
		if c.account != user || c.path != "/v1/chat/completions" || c.ua != "magpie-gemini-warmup/1" {
			t.Errorf("a first call went as %+v, want the account pinned and magpie's own user agent", c)
		}
		asked[c.model] = true
	}
	for pool, model := range want {
		if !asked[model] {
			t.Errorf("no call was made with %q, the model of the %s pool: %+v", model, pool, calls())
		}
		if ps := gemPoolState(t, user, pool); ps.Model != model || ps.Attempts != 1 || ps.Status != http.StatusOK || ps.LastCallAt.IsZero() || ps.Err != "" {
			t.Errorf("the %s pool is kept as %+v, want one call with %q", pool, ps, model)
		}
	}
	// each pool's own state, not the account's: the account is one row
	if st := gemStateOf(t, user); len(st.Pools) != 3 || st.LastCheck.IsZero() || !st.NextAt.After(gemAt(0)) {
		t.Errorf("the account is kept as %+v, want three pools and a wait", st)
	}

	// the Claude & GPT pool has been drawn on since, its own 5 hours alone:
	// it is open, and its two windows are both pinned. The other two are
	// still at nothing — a call that drew on no pool, which is the case this
	// exists for — so they are opened again.
	read = gemAllowance(user, &reset,
		gemPoolReading{pool: gemPool},
		gemPoolReading{pool: gemClaudePool, five: 0.0015, week: 0.0008},
		gemPoolReading{pool: gemArgonPool, noFive: true})
	gemWarmNow(context.Background(), gemAt(20))
	if len(calls()) != 5 {
		t.Fatalf("an open pool was called: %+v", calls())
	}
	if ps := gemPoolState(t, user, gemClaudePool); ps.Attempts != 1 || ps.Err != "" {
		t.Errorf("a pool already drawing on itself was kept as %+v", ps)
	}
	if ps := gemPoolState(t, user, gemArgonPool); ps.Attempts != 2 {
		t.Errorf("the Argon pool was called %d times, want it tried again while it is not open", ps.Attempts)
	}
}

// A pool whose 5-hour window has reset to nothing is not open because its
// 7-day one still carries use: the test is each window's own, on its own
// pool.
func TestGeminiWarmReadsEachWindowOfAPoolOnItsOwn(t *testing.T) {
	gemHome(t)
	gemReset(t, true, 5*time.Minute)
	const user = "y15570087076@gmail.com"
	gemSaveModels(t, user, gemList("gemini-3.8-flash", "claude-sonnet-5-5")...)
	srv, calls := gemGateway(t, 0, "")
	SetGeminiProbeURL(srv.URL)

	reset := gemAt(30)
	read := gemAllowance(user, &reset,
		gemPoolReading{pool: gemPool, five: 0, week: 91},
		gemPoolReading{pool: gemClaudePool, five: 0, week: 62})
	LoginUsageVia(func(context.Context, string) map[string]SubscriptionQuota {
		return map[string]SubscriptionQuota{user: read}
	})
	t.Cleanup(func() { LoginUsageVia(nil) })

	gemWarmNow(context.Background(), gemAt(0))
	if len(calls()) != 2 {
		t.Fatalf("the look sent %+v, want both pools opened: their 5 hours are at 0", calls())
	}
}

// A first call is one request of a client's, shaped as one: max_tokens sent,
// answered whole, the model the pool's own.
func TestGeminiWarmOneFirstCall(t *testing.T) {
	gemHome(t)
	gemReset(t, true, 5*time.Minute)
	const user = "a@example.com"
	gemSaveModels(t, user, gemList("gemini-2.5-flash-lite", "gemini-3.8-flash", "claude-sonnet-5-5")...)
	srv, calls := gemGateway(t, 0, "")
	SetGeminiProbeURL(srv.URL + "/")

	reset := gemAt(30)
	LoginUsageVia(func(context.Context, string) map[string]SubscriptionQuota {
		return map[string]SubscriptionQuota{user: gemTwoPools(user, &reset)}
	})
	t.Cleanup(func() { LoginUsageVia(nil) })

	gemWarmNow(context.Background(), gemAt(0))
	if len(calls()) != 2 {
		t.Fatalf("an unused allowance got %d first calls, want one a pool: %+v", len(calls()), calls())
	}
	var gemini gemCall
	for _, c := range calls() {
		if c.model == "gemini-3.8-flash" {
			gemini = c
		}
	}
	if want := "/v1/chat/completions " + user + " magpie-gemini-warmup/1"; gemini.path+" "+gemini.account+" "+gemini.ua != want {
		t.Errorf("the first call was %q, want %q", gemini.path+" "+gemini.account+" "+gemini.ua, want)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(gemini.body), &sent); err != nil {
		t.Fatalf("the first call's body: %v in %q", err, gemini.body)
	}
	// max_tokens is magpie's contract with a client, not a bound on this
	// reply: Antigravity's Gemini drops it (gateway/codeassist.go sets
	// maxOutputTokens only for a vendor that isn't Antigravity or for its
	// Claude), so a live first call answers in ~10 tokens whatever is sent.
	// The field is asserted as sent; what the vendor does with it isn't.
	if sent["model"] != "gemini-3.8-flash" || sent["max_tokens"] != float64(1) || sent["stream"] != false {
		t.Errorf("the first call sent %v, want the family that draws on the pool, max_tokens sent, answered whole", sent)
	}
}

// A first call the vendor refuses is kept against that pool, with its status
// and one line of what it said, counted as a request the account saw, and
// tried again next look: the window is still not started.
func TestGeminiWarmRefusalKeptAndTriedAgain(t *testing.T) {
	gemHome(t)
	gemReset(t, true, 5*time.Minute)
	const user = "a@example.com"
	gemSaveModels(t, user, gemList("gemini-2.5-flash-lite")...)
	srv, calls := gemGateway(t, http.StatusTooManyRequests,
		"{\"error\":{\"message\":\"this account rests until 10:00\"}}\nand a second line\n")
	SetGeminiProbeURL(srv.URL)

	reset := gemAt(30)
	LoginUsageVia(func(context.Context, string) map[string]SubscriptionQuota {
		return map[string]SubscriptionQuota{user: gemAllowance(user, &reset, gemPoolReading{pool: gemPool})}
	})
	t.Cleanup(func() { LoginUsageVia(nil) })

	gemWarmNow(context.Background(), gemAt(0))
	ps := gemPoolState(t, user, gemPool)
	if ps.Status != http.StatusTooManyRequests || ps.Attempts != 1 {
		t.Errorf("the refusal was kept as %+v, want a 429 counted once", ps)
	}
	if !ps.LastCallAt.IsZero() {
		t.Errorf("a refused call was kept as one that started the window: %v", ps.LastCallAt)
	}
	if strings.Contains(ps.Err, "\n") || !strings.Contains(ps.Err, "rests until 10:00") {
		t.Errorf("the refusal reads %q, want one line of what the vendor said", ps.Err)
	}
	gemWarmNow(context.Background(), gemAt(20))
	if len(calls()) != 2 || gemPoolState(t, user, gemPool).Attempts != 2 {
		t.Errorf("a refused first call was made %d times, %d counted; want it tried again", len(calls()), gemPoolState(t, user, gemPool).Attempts)
	}
}

// With the watching off nothing is read and nothing is called: the loop
// runs either way.
func TestGeminiWarmOffAsksNothing(t *testing.T) {
	gemHome(t)
	gemReset(t, false, 5*time.Minute)
	var read int
	LoginUsageVia(func(context.Context, string) map[string]SubscriptionQuota {
		read++
		return map[string]SubscriptionQuota{"a@example.com": gemTwoPools("a@example.com", nil)}
	})
	t.Cleanup(func() { LoginUsageVia(nil) })
	srv, calls := gemGateway(t, 0, "")
	SetGeminiProbeURL(srv.URL)

	gemWarmNow(context.Background(), gemAt(0))
	if read != 0 {
		t.Errorf("the watching is off and the allowance was read %d times", read)
	}
	if len(calls()) != 0 {
		t.Errorf("the watching is off and %d first calls were sent", len(calls()))
	}
}

// A reading kept from an earlier look is never taken for "nothing used":
// with allowances read only when asked, that reading would spend a first
// call on every look, for every pool.
func TestGeminiWarmKeptReadingMakesNoCall(t *testing.T) {
	gemHome(t)
	gemReset(t, true, 5*time.Minute)
	const user = "a@example.com"
	gemSaveModels(t, user, gemList("gemini-3.8-flash", "claude-sonnet-5-5")...)
	srv, calls := gemGateway(t, 0, "")
	SetGeminiProbeURL(srv.URL)

	reset := gemAt(30)
	asOf := gemAt(0).Add(-time.Hour)
	LoginUsageVia(func(context.Context, string) map[string]SubscriptionQuota {
		q := gemThreePools(user, &reset)
		q.AsOf = &asOf
		return map[string]SubscriptionQuota{user: q}
	})
	t.Cleanup(func() { LoginUsageVia(nil) })

	gemWarmNow(context.Background(), gemAt(0))
	if len(calls()) != 0 {
		t.Errorf("a kept reading spent %d first calls", len(calls()))
	}
	for _, pool := range []string{gemPool, gemClaudePool, gemArgonPool} {
		if ps := gemPoolState(t, user, pool); !strings.Contains(ps.Err, "last read") {
			t.Errorf("the %s pool was kept as %q, want it to say the reading is one kept", pool, ps.Err)
		}
	}
}

// A bucket the summary gave no figure for is no reading of "nothing used"
// either: no first call is made on it, and the page is told why rather than
// shown a window at 100.
func TestGeminiWarmNoFigureMakesNoCall(t *testing.T) {
	gemHome(t)
	gemReset(t, true, 5*time.Minute)
	const user = "a@example.com"
	gemSaveModels(t, user, gemList("gemini-3.8-flash")...)
	srv, calls := gemGateway(t, 0, "")
	SetGeminiProbeURL(srv.URL)

	LoginUsageVia(func(context.Context, string) map[string]SubscriptionQuota {
		return map[string]SubscriptionQuota{user: {Provider: "antigravity", User: user,
			Windows: []QuotaWindow{
				gemBucket(gemPool, "5 hours", 5*time.Hour, 100, nil),
				gemBucket(gemPool, "7 days", 7*24*time.Hour, 100, nil),
			}}}
	})
	t.Cleanup(func() { LoginUsageVia(nil) })

	gemWarmNow(context.Background(), gemAt(0))
	if len(calls()) != 0 {
		t.Errorf("a window with no figure spent %d first calls", len(calls()))
	}
	if ps := gemPoolState(t, user, gemPool); !strings.Contains(ps.Err, "no figure") {
		t.Errorf("the pool was kept as %q, want it to say the vendor gave no figure", ps.Err)
	}
}

// A reading that failed is kept with what it said and looked at again next
// time, never in a spin, and says nothing about the pools: what they were is
// what they stay.
func TestGeminiWarmReadErrorKept(t *testing.T) {
	gemHome(t)
	gemReset(t, true, 5*time.Minute)
	stood := false
	LoginUsageVia(func(context.Context, string) map[string]SubscriptionQuota {
		stood = true
		return map[string]SubscriptionQuota{"a@example.com": {Provider: "antigravity", User: "a@example.com",
			Windows: []QuotaWindow{}, Error: "429 too many requests\nretry later"}}
	})
	t.Cleanup(func() { LoginUsageVia(nil) })
	gemWarmNow(context.Background(), gemAt(0))
	// loginUsageAt (login_usage.go) answers before its stand-in is asked when
	// accounts are left unread: a look then reads nothing, which this test
	// would otherwise report as a reading that came back empty. Naming the
	// gate is what tells the two apart.
	if !stood {
		t.Fatalf("the stand-in was never asked: watching %v, discovery off %v, antigravity left alone %v",
			gemWatched(), settings.AccountDiscoveryOff(), leftAlone("antigravity"))
	}
	st := gemStateOf(t, "a@example.com")
	// one line of it, as the page shows one: a vendor's refusal carries a
	// whole page of prose after its first line
	if st.Err != "429 too many requests" {
		t.Errorf("a failed reading was kept as %q", st.Err)
	}
	if len(st.Pools) != 0 || !st.NextAt.After(gemAt(0)) {
		t.Errorf("the account is kept as %+v, want no pool touched and a wait", st)
	}
}

// A pool magpie knows no model of is shown on the page and never called: a
// call to a model that draws on another pool would open that one instead,
// and count as a first call that started nothing.
func TestGeminiWarmUnwatchedPoolMakesNoCall(t *testing.T) {
	gemHome(t)
	gemReset(t, true, 5*time.Minute)
	const user = "a@example.com"
	gemSaveModels(t, user, gemList("gemini-3.8-flash")...)
	srv, calls := gemGateway(t, 0, "")
	SetGeminiProbeURL(srv.URL)

	reset := gemAt(30)
	LoginUsageVia(func(context.Context, string) map[string]SubscriptionQuota {
		return map[string]SubscriptionQuota{user: gemAllowance(user, &reset,
			gemPoolReading{pool: gemPool, five: 18.8},
			gemPoolReading{pool: "Gemini 5 Lyra", noFive: true, week: 1})}
	})
	t.Cleanup(func() { LoginUsageVia(nil) })

	gemWarmNow(context.Background(), gemAt(0))
	if len(calls()) != 0 {
		t.Errorf("a pool magpie knows no model of was called: %+v", calls())
	}
	if ps := gemPoolState(t, user, "Gemini 5 Lyra"); !strings.Contains(ps.Err, "isn't watched") || ps.Attempts != 0 {
		t.Errorf("the pool is kept as %+v, want it recorded as not watched", ps)
	}
	rows := GeminiRows(gemAt(1))
	if len(rows) != 1 || len(rows[0].Pools) != 2 {
		t.Fatalf("the page is shown %+v, want the account with both pools", rows)
	}
	var lyra *GeminiPool
	for i := range rows[0].Pools {
		if rows[0].Pools[i].Pool == "Gemini 5 Lyra" {
			lyra = &rows[0].Pools[i]
		}
	}
	if lyra == nil || lyra.Watched || len(lyra.Windows) != 1 {
		t.Errorf("the page shows %+v for the unwatched pool, want it drawn and marked", lyra)
	}
}

// A pool whose model the account doesn't list is recorded and left alone: a
// call with another pool's model would open that pool instead, and count as
// a first call that started nothing.
func TestGeminiWarmPoolWithoutItsModelMakesNoCall(t *testing.T) {
	gemHome(t)
	gemReset(t, true, 5*time.Minute)
	const user = "y19913868365@gmail.com"
	// nothing of the Claude & GPT pool on this list
	gemSaveModels(t, user, gemList("gemini-3.8-flash", "gemini-4-argon")...)
	srv, calls := gemGateway(t, 0, "")
	SetGeminiProbeURL(srv.URL)

	reset := gemAt(30)
	// the reading names no model of that pool either: this is an account
	// whose plan has none, not one whose catalogue is behind
	read := gemWithoutModels(gemThreePools(user, &reset), "claude")
	LoginUsageVia(func(context.Context, string) map[string]SubscriptionQuota {
		return map[string]SubscriptionQuota{user: read}
	})
	t.Cleanup(func() { LoginUsageVia(nil) })

	gemWarmNow(context.Background(), gemAt(0))
	if len(calls()) != 2 {
		t.Fatalf("the look sent %+v, want the Gemini and Argon pools alone", calls())
	}
	if ps := gemPoolState(t, user, gemClaudePool); ps.Attempts != 0 || ps.Model != "" || !strings.Contains(ps.Err, "no model") {
		t.Errorf("the Claude & GPT pool is kept as %+v, want it left alone with the reason", ps)
	}
}

// The first look of a process can run before the catalogue has been read:
// the allowance the look just took names each model's id, and that is enough
// to open a pool the plan lists. Antigravity's levels of a family still come
// back as the family magpie offers.
func TestGeminiModelFallsBackToTheReadingsModels(t *testing.T) {
	gemHome(t)
	const user = "a@example.com"
	// nothing fetched from the vendor yet
	if err := catalog.SaveLive("antigravity", "", nil); err != nil {
		t.Fatal(err)
	}
	q := SubscriptionQuota{Provider: "antigravity", User: user, Windows: []QuotaWindow{
		{Name: "Gemini 3.8 Flash (High)", Model: "gemini-3.8-flash-high", Family: "Gemini", Pool: gemPool},
		{Name: "Gemini 3.8 Flash (Low)", Model: "gemini-3.8-flash-low", Family: "Gemini", Pool: gemPool},
		{Name: "Claude Sonnet 5.5", Model: "claude-sonnet-5-5", Family: "Claude", Pool: gemClaudePool},
		{Name: "Gemini 4 Argon", Model: "gemini-4-argon", Pool: gemArgonPool},
	}}
	for _, c := range []struct{ pool, want string }{
		{gemPool, "gemini-3.8-flash"},
		{gemClaudePool, "claude-sonnet-5-5"},
		{gemArgonPool, "gemini-4-argon"},
	} {
		if got := gemModelFor(user, c.pool, q); got != c.want {
			t.Errorf("gemModelFor(%q) with no catalogue = %q, want %q", c.pool, got, c.want)
		}
	}
	// a pool the reading names no model of is still left alone
	if got := gemModelFor(user, "Gemini 5 Lyra", q); got != "" {
		t.Errorf("a pool nobody knows got %q, want none", got)
	}
}

// The page's Refresh reads the allowances now and lists the accounts with
// their pools, even with the watching off and before any look has been made:
// the look of its own is minutes away, and the minute's cache would answer
// with a reading taken before a window reset.
func TestGeminiRefreshReadsNow(t *testing.T) {
	gemHome(t)
	gemReset(t, false, 5*time.Minute) // the watching is off
	const user = "a@example.com"
	if err := addGoogleLogin("antigravity", user, "", googleAuth{
		AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	var reads int
	reset := gemAt(30)
	LoginUsageVia(func(context.Context, string) map[string]SubscriptionQuota {
		reads++
		return map[string]SubscriptionQuota{user: gemTwoPools(user, &reset)}
	})
	t.Cleanup(func() { LoginUsageVia(nil) })
	// a reading taken a moment ago is what the page must not be answered with
	key := "antigravity/" + user
	loginUsageCache.Lock()
	loginUsageCache.m = map[string]loginUsageEntry{key: {at: time.Now()}}
	loginUsageCache.Unlock()
	t.Cleanup(func() {
		loginUsageCache.Lock()
		loginUsageCache.m = nil
		loginUsageCache.Unlock()
	})

	if rows := GeminiRows(gemAt(1)); len(rows) != 0 {
		t.Fatalf("the page lists %+v before any reading, want nothing", rows)
	}
	RefreshGemini(context.Background(), gemAt(1))
	if reads != 1 {
		t.Fatalf("Refresh read the allowances %d times, want 1", reads)
	}
	rows := GeminiRows(gemAt(1))
	if len(rows) != 1 || rows[0].User != user || len(rows[0].Pools) != 2 {
		t.Fatalf("the page is shown %+v, want the account and its two pools", rows)
	}
	if ws := rows[0].Pools[1].Windows; len(ws) != 2 || ws[0].Name != "5 hours" || ws[1].Name != "7 days" {
		t.Errorf("the pools' windows are %+v, want both of the Gemini pool's", ws)
	}
	// the cache's entry was forgotten first, so the read is the vendor's now
	loginUsageCache.Lock()
	at := loginUsageCache.m[key].at
	loginUsageCache.Unlock()
	if !at.IsZero() {
		t.Error("the reading kept from the minute's cache was not forgotten")
	}
	// and every Refresh is a reading of its own
	RefreshGemini(context.Background(), gemAt(2))
	if reads != 2 {
		t.Errorf("a second Refresh read the allowances %d times, want 2", reads)
	}
}

// A look right after a window reset does not read the minute's cache: it
// holds a reading from before the reset, which says the pool is in use, so
// the pool that has started over would be opened a whole interval later.
func TestGeminiWarmForgetsAReadingThatHasReset(t *testing.T) {
	gemHome(t)
	gemReset(t, true, 5*time.Minute)
	const user = "a@example.com"
	if err := addGoogleLogin("antigravity", user, "", googleAuth{
		AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	reset := gemAt(30)
	LoginUsageVia(func(context.Context, string) map[string]SubscriptionQuota {
		return map[string]SubscriptionQuota{user: gemAllowance(user, &reset, gemPoolReading{pool: gemPool, five: 18.8})}
	})
	t.Cleanup(func() { LoginUsageVia(nil) })
	key := "antigravity/" + user
	loginUsageCache.Lock()
	loginUsageCache.m = map[string]loginUsageEntry{key: {at: time.Now()}}
	loginUsageCache.Unlock()
	t.Cleanup(func() {
		loginUsageCache.Lock()
		loginUsageCache.m = nil
		loginUsageCache.Unlock()
	})

	// a window that has already reset, and one that has not
	past := gemAt(0).Add(-time.Minute)
	future := gemAt(0).Add(time.Minute)
	if !gemResetSince(map[string]SubscriptionQuota{user: gemAllowance(user, &past, gemPoolReading{pool: gemPool})}, gemAt(0)) {
		t.Error("a window whose reset has passed was not seen as reset")
	}
	if gemResetSince(map[string]SubscriptionQuota{user: gemAllowance(user, &future, gemPoolReading{pool: gemPool})}, gemAt(0)) {
		t.Error("a window still to reset was read as reset")
	}

	gemWarmMu.Lock()
	gemWarmMu.read = map[string]SubscriptionQuota{user: gemAllowance(user, &past, gemPoolReading{pool: gemPool, five: 18.8})}
	gemWarmMu.Unlock()
	gemWarmNow(context.Background(), gemAt(0))
	loginUsageCache.Lock()
	at := loginUsageCache.m[key].at
	loginUsageCache.Unlock()
	if !at.IsZero() {
		t.Error("a reading from before a window reset was kept for the look")
	}
}

// The page's rows are the accounts watched, by user, each with its pools and
// the windows the last look read of them; nothing is asked of the vendor for
// one.
func TestGeminiRowsWithTheirPools(t *testing.T) {
	gemHome(t)
	gemReset(t, true, 5*time.Minute)
	reset := gemAt(30)
	gemWarmMu.Lock()
	gemWarmMu.state = map[string]gemWarm{
		"b@example.com": {User: "b@example.com", LastCheck: gemAt(1), NextAt: gemAt(4), Err: "rests"},
		"a@example.com": {User: "a@example.com", LastCheck: gemAt(1), NextAt: gemAt(4), Pools: map[string]gemPoolWarm{
			gemPool: {Pool: gemPool, Model: "gemini-3.8-flash", LastCheck: gemAt(1), NextAt: gemAt(4), LastCallAt: gemAt(1), Attempts: 2, Status: 429, Err: "rests"},
		}},
	}
	gemWarmMu.read = map[string]SubscriptionQuota{"a@example.com": gemThreePools("a@example.com", &reset)}
	gemWarmMu.Unlock()

	rows := GeminiRows(gemAt(2))
	if len(rows) != 2 || rows[0].User != "a@example.com" || rows[1].User != "b@example.com" {
		t.Fatalf("the rows are %+v, want them by user", rows)
	}
	if len(rows[0].Pools) != 3 || rows[0].Pools[0].Pool != gemClaudePool || rows[0].Pools[2].Pool != gemArgonPool {
		t.Fatalf("a's pools are %+v, want each of its three", rows[0].Pools)
	}
	if got := rows[0].Pools[1]; got.Pool != gemPool || got.Model != "gemini-3.8-flash" || got.Attempts != 2 || got.Status != 429 ||
		got.Err != "rests" || !got.LastCallAt.Equal(gemAt(1)) || len(got.Windows) != 2 {
		t.Errorf("the Gemini pool's row is %+v", got)
	}
	// the weekly-only pool is drawn with the one window it has
	if got := rows[0].Pools[2]; len(got.Windows) != 1 || got.Windows[0].Name != "7 days" || !got.Watched {
		t.Errorf("the Argon pool's row is %+v, want its 7 days alone", got)
	}
	if !rows[0].NextAt.Equal(gemAt(4)) || !rows[0].LastCheck.Equal(gemAt(1)) {
		t.Errorf("a's row is %+v", rows[0])
	}
	if rows[1].Err != "rests" || len(rows[1].Pools) != 0 {
		t.Errorf("b's row is %+v, want its error and no reading", rows[1])
	}

	// with the watching off nothing is scheduled, and a look that has come
	// due is drawn as now rather than as a time gone by
	SetGeminiWatch(GeminiWatch{})
	if rows := GeminiRows(gemAt(2)); !rows[0].NextAt.IsZero() || !rows[0].Pools[1].NextAt.IsZero() {
		t.Errorf("the watching is off and a's next look is %v, %v", rows[0].NextAt, rows[0].Pools[1].NextAt)
	}
	SetGeminiWatch(GeminiWatchOf(true, 0))
	if rows := GeminiRows(gemAt(9)); !rows[0].NextAt.Equal(gemAt(9)) || !rows[0].Pools[1].NextAt.Equal(gemAt(9)) {
		t.Errorf("a look that came due is drawn as %v, %v, want now", rows[0].NextAt, rows[0].Pools[1].NextAt)
	}
}

// What a look keeps is written whole beside providers.json, a pool at a
// time, and read back by the next magpie.
func TestGeminiStateKeptOnDisk(t *testing.T) {
	gemHome(t)
	gemReset(t, true, 5*time.Minute)
	path := gemWarmPath()
	if filepath.Base(path) != "gemini-warmup.json" || filepath.Dir(path) != filepath.Dir(Path()) {
		t.Fatalf("the watching is kept at %q, want gemini-warmup.json beside providers.json", path)
	}
	writeGemWarmState(path, map[string]gemWarm{"a@example.com": {User: "a@example.com", LastCheck: gemAt(1), NextAt: gemAt(6), Pools: map[string]gemPoolWarm{
		gemPool:       {Pool: gemPool, Model: "gemini-3.8-flash", LastCheck: gemAt(1), NextAt: gemAt(6), LastCallAt: gemAt(1), Attempts: 3, Status: 200},
		gemClaudePool: {Pool: gemClaudePool, Model: "claude-sonnet-5-5", LastCheck: gemAt(1), NextAt: gemAt(6), Attempts: 1, Status: 429, Err: "rests"},
		gemArgonPool:  {Pool: gemArgonPool, Model: "gemini-4-argon", LastCallAt: gemAt(1), Attempts: 1, Status: 200},
	}}})
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the state file is %v, %v; want it readable by the user alone", fi, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temporary file was left behind")
	}
	got := readGemWarmState(path)["a@example.com"]
	if !got.NextAt.Equal(gemAt(6)) || len(got.Pools) != 3 {
		t.Fatalf("what was read back is %+v", got)
	}
	if ps := got.poolState(gemPool); ps.Model != "gemini-3.8-flash" || ps.Attempts != 3 || !ps.NextAt.Equal(gemAt(6)) || ps.Status != 200 {
		t.Errorf("the Gemini pool read back as %+v", ps)
	}
	if ps := got.poolState(gemClaudePool); ps.Model != "claude-sonnet-5-5" || ps.Attempts != 1 || ps.Err != "rests" || ps.Status != 429 {
		t.Errorf("the Claude & GPT pool read back as %+v", ps)
	}
	if ps := got.poolState(gemArgonPool); ps.Model != "gemini-4-argon" || !ps.LastCallAt.Equal(gemAt(1)) {
		t.Errorf("the Argon pool read back as %+v", ps)
	}
	if len(readGemWarmState(filepath.Join(t.TempDir(), "gone.json"))) != 0 {
		t.Error("a file that isn't there was read as a state")
	}
	// a file written before the pools were kept apart reads with none of
	// them, and the next look gives each its own
	old := filepath.Join(t.TempDir(), "old.json")
	if err := os.WriteFile(old, []byte(`{"a@example.com":{"user":"a@example.com","model":"gemini-2.5-flash-lite","attempts":3}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := readGemWarmState(old)["a@example.com"]; st.User != "a@example.com" || len(st.Pools) != 0 {
		t.Errorf("an older state file read as %+v, want the account and no pools", st)
	}
}

// A look is made when an account's wait has passed, and the accounts are
// listed again every half hour for one signed in since.
func TestGeminiDue(t *testing.T) {
	now := gemAt(0)
	if due, rescan := gemDue(nil, now, now); due || rescan {
		t.Errorf("nothing kept and just listed: due %v, rescan %v", due, rescan)
	}
	if due, rescan := gemDue(nil, now, time.Time{}); due || !rescan {
		t.Errorf("never listed: due %v, rescan %v", due, rescan)
	}
	if due, _ := gemDue(map[string]gemWarm{"a": {NextAt: now.Add(-time.Second)}}, now, now); !due {
		t.Error("a wait that has passed is not due")
	}
	if due, _ := gemDue(map[string]gemWarm{"a": {NextAt: now.Add(time.Minute)}}, now, now); due {
		t.Error("a wait still to run is due")
	}
	if _, rescan := gemDue(nil, now, now.Add(-gemScanEvery)); !rescan {
		t.Error("the accounts are not listed again after half an hour")
	}
}
