package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/agentenv"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// The Gemini page's mode and interval: what it reads, what it may set, and
// what it may not — an interval under the shortest one is refused rather
// than rounded up, so the page never makes the watching ask faster than it
// was allowed to.
func TestGeminiCustomRoutes(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(h, ".cache"))
	t.Setenv("APPDATA", filepath.Join(h, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(h, "AppData", "Local"))
	for _, v := range agentenv.Vars {
		t.Setenv(v, "")
	}
	handler := Handler(nil, nil)

	type state struct {
		On      bool                 `json:"on"`
		Every   int                  `json:"every"`
		Default int                  `json:"default"`
		Min     int                  `json:"min"`
		Rows    []provider.GeminiRow `json:"rows"`
	}
	read := func() state {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", "/api/gemini/custom", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET: %d %s", rec.Code, rec.Body)
		}
		var s state
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
			t.Fatalf("GET: %v in %s", err, rec.Body)
		}
		// the page draws its empty state from a list, so a null rows would
		// read as a page that failed
		if s.Rows == nil {
			t.Fatalf("GET: rows is null, want a list: %s", rec.Body)
		}
		return s
	}
	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("POST", "/api/gemini/custom", strings.NewReader(body)))
		return rec
	}

	s := read()
	if s.On || s.Every != 0 {
		t.Fatalf("fresh settings: on=%v every=%d, want off and magpie's own", s.On, s.Every)
	}
	if want := int(provider.GeminiWatchDefault().Seconds()); s.Default != want {
		t.Fatalf("default %d, want %d", s.Default, want)
	}
	min := int(provider.GeminiWatchMin().Seconds())
	if s.Min != min {
		t.Fatalf("min %d, want %d", s.Min, min)
	}

	// on, with an interval the page may set
	rec := post(`{"on":true,"every":600}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST on: %d %s", rec.Code, rec.Body)
	}
	var got state
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("POST on: %v in %s", err, rec.Body)
	}
	if !got.On || got.Every != 600 {
		t.Fatalf("POST on answered on=%v every=%d, want true and 600", got.On, got.Every)
	}
	if saved := settings.Load().GeminiWatch; !saved.On || saved.Every != 600 {
		t.Fatalf("saved %+v, want on and 600", saved)
	}
	if again := read(); !again.On || again.Every != 600 {
		t.Fatalf("read back on=%v every=%d, want true and 600", again.On, again.Every)
	}

	// zero is magpie's own interval, which the watching takes as none set
	if rec := post(`{"on":true,"every":0}`); rec.Code != http.StatusOK {
		t.Fatalf("POST every 0: %d %s", rec.Code, rec.Body)
	}
	if saved := settings.Load().GeminiWatch; !saved.On || saved.Every != 0 {
		t.Fatalf("every 0 saved %+v, want on and 0", saved)
	}

	// under the shortest one is refused, and nothing is saved
	rec = post(`{"on":true,"every":` + strconv.Itoa(min-1) + `}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST under min: %d %s, want 400", rec.Code, rec.Body)
	}
	if want := "every is at least " + strconv.Itoa(min) + " seconds"; strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("POST under min said %q, want %q", rec.Body.String(), want)
	}
	if saved := settings.Load().GeminiWatch; !saved.On || saved.Every != 0 {
		t.Fatalf("a refused interval saved %+v, want the last good one", saved)
	}

	// off, which keeps the interval as it was set
	if rec := post(`{"on":false,"every":1800}`); rec.Code != http.StatusOK {
		t.Fatalf("POST off: %d %s", rec.Code, rec.Body)
	}
	if saved := settings.Load().GeminiWatch; saved.On || saved.Every != 1800 {
		t.Fatalf("off saved %+v, want off and 1800", saved)
	}

	// asked=1 is the page's Refresh: it reads the allowances now and answers
	// with the rows, rather than failing or holding the request. No account
	// is signed in here, so the rows are the empty list the page draws from.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/api/gemini/custom?asked=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET asked=1: %d %s", rec.Code, rec.Body)
	}
	var asked state
	if err := json.Unmarshal(rec.Body.Bytes(), &asked); err != nil {
		t.Fatalf("GET asked=1: %v in %s", err, rec.Body)
	}
	if asked.Rows == nil || len(asked.Rows) != 0 {
		t.Fatalf("GET asked=1 rows %+v, want the empty list", asked.Rows)
	}
}
