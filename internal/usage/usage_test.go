package usage

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// canned mirrors the real endpoint shape (fake values); most of the many
// sibling windows the API returns are elided since the parser ignores them.
const canned = `{
  "five_hour":      {"utilization": 42.5, "resets_at": "2026-07-08T18:30:00.000000+00:00"},
  "seven_day":      {"utilization": 12.0, "resets_at": "2026-07-14T00:00:00.000000+00:00"},
  "seven_day_opus": {"utilization": 88.0, "resets_at": "2026-07-14T00:00:00.000000+00:00"},
  "seven_day_sonnet": null,
  "spend": {"percent": 0}
}`

func TestParseUsage(t *testing.T) {
	snap, err := parseUsage(strings.NewReader(canned))
	if err != nil {
		t.Fatalf("parseUsage: %v", err)
	}
	if snap.FiveHour == nil || snap.FiveHour.Utilization != 42.5 {
		t.Fatalf("five_hour = %+v, want util 42.5", snap.FiveHour)
	}
	want, _ := time.Parse(time.RFC3339, "2026-07-08T18:30:00+00:00")
	if !snap.FiveHour.ResetsAt.Equal(want) {
		t.Errorf("five_hour resets_at = %v, want %v", snap.FiveHour.ResetsAt, want)
	}
	if snap.SevenDayOpus == nil || snap.SevenDayOpus.Utilization != 88.0 {
		t.Fatalf("seven_day_opus = %+v, want util 88", snap.SevenDayOpus)
	}
}

// A null (or absent) window must decode to a nil *Window, not a zero one, so
// callers can tell "not offered on this plan" from "0% used".
func TestParseUsageNullWindow(t *testing.T) {
	snap, err := parseUsage(strings.NewReader(`{"five_hour": {"utilization": 1.0, "resets_at": ""}, "seven_day": null}`))
	if err != nil {
		t.Fatalf("parseUsage: %v", err)
	}
	if snap.SevenDay != nil {
		t.Errorf("seven_day = %+v, want nil", snap.SevenDay)
	}
	if snap.SevenDayOpus != nil {
		t.Errorf("seven_day_opus = %+v, want nil", snap.SevenDayOpus)
	}
	// An empty resets_at is tolerated and leaves the time zero.
	if snap.FiveHour == nil || !snap.FiveHour.ResetsAt.IsZero() {
		t.Errorf("five_hour = %+v, want present with zero ResetsAt", snap.FiveHour)
	}
}

func TestBinding(t *testing.T) {
	tests := []struct {
		name      string
		snap      Snapshot
		wantLimit Limit
		wantUtil  float64
	}{
		{
			name:      "opus is highest",
			snap:      Snapshot{FiveHour: &Window{Utilization: 42}, SevenDay: &Window{Utilization: 12}, SevenDayOpus: &Window{Utilization: 88}},
			wantLimit: LimitOpus,
			wantUtil:  88,
		},
		{
			name:      "session is highest",
			snap:      Snapshot{FiveHour: &Window{Utilization: 90}, SevenDay: &Window{Utilization: 12}},
			wantLimit: LimitSession,
			wantUtil:  90,
		},
		{
			// Equal utilizations resolve to the shortest (soonest-to-reset)
			// window.
			name:      "tie prefers session over week",
			snap:      Snapshot{FiveHour: &Window{Utilization: 50}, SevenDay: &Window{Utilization: 50}},
			wantLimit: LimitSession,
			wantUtil:  50,
		},
		{
			name:      "only week present",
			snap:      Snapshot{SevenDay: &Window{Utilization: 33}},
			wantLimit: LimitWeek,
			wantUtil:  33,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, limit := tc.snap.Binding()
			if limit != tc.wantLimit {
				t.Errorf("limit = %v, want %v", limit, tc.wantLimit)
			}
			if w == nil || w.Utilization != tc.wantUtil {
				t.Errorf("window = %+v, want util %v", w, tc.wantUtil)
			}
		})
	}
}

func TestBindingEmpty(t *testing.T) {
	w, limit := Snapshot{}.Binding()
	if w != nil || limit != LimitNone {
		t.Errorf("Binding() = (%+v, %v), want (nil, LimitNone)", w, limit)
	}
}

func TestTokenFromJSON(t *testing.T) {
	tok, err := tokenFromJSON([]byte(`{"claudeAiOauth": {"accessToken": "sk-fake-abc"}}`))
	if err != nil {
		t.Fatalf("tokenFromJSON: %v", err)
	}
	if tok != "sk-fake-abc" {
		t.Errorf("token = %q, want sk-fake-abc", tok)
	}

	if _, err := tokenFromJSON([]byte(`{"claudeAiOauth": {"accessToken": ""}}`)); err != ErrNoCredentials {
		t.Errorf("empty token err = %v, want ErrNoCredentials", err)
	}
}

func TestParseCodexUsage(t *testing.T) {
	snap, err := parseCodexUsage([]byte(`{
  "rateLimits": {"primary": null, "secondary": null},
  "rateLimitsByLimitId": {
    "codex": {
      "primary": {"usedPercent": 37, "windowDurationMins": 300, "resetsAt": 1784680000},
      "secondary": {"usedPercent": 62, "windowDurationMins": 10080, "resetsAt": 1785000000}
    }
  }
}`))
	if err != nil {
		t.Fatalf("parseCodexUsage: %v", err)
	}
	if len(snap.Named) != 2 {
		t.Fatalf("named windows = %+v, want two", snap.Named)
	}
	if got := snap.Named[0]; got.Label != "session" || !got.Session || got.Window.Utilization != 37 {
		t.Errorf("primary window = %+v, want 5h session at 37%%", got)
	}
	if got := snap.Named[1]; got.Label != "week" || got.ShortLabel != "wk" || got.Window.Utilization != 62 {
		t.Errorf("secondary window = %+v, want week at 62%%", got)
	}
	binding, ok := snap.BindingWindow()
	if !ok || binding.Label != "week" {
		t.Errorf("binding window = %+v, %v; want week", binding, ok)
	}
}

func TestFetchCodex(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := FetchCodex(ctx, os.Args[0], []string{
		"-test.run=TestCodexAppServerHelper", "--", "codex-usage-helper",
	})
	if err != nil {
		t.Fatalf("FetchCodex: %v", err)
	}
	if len(snap.Named) != 2 || snap.Named[0].Window.Utilization != 41 || snap.Named[1].Window.Utilization != 73 {
		t.Errorf("snapshot = %+v, want helper response", snap)
	}
	if snap.FetchedAt.IsZero() {
		t.Error("FetchedAt should be populated")
	}
}

// TestCodexAppServerHelper becomes a tiny stdio App Server only when spawned
// by TestFetchCodex. In the parent test process it returns immediately.
func TestCodexAppServerHelper(t *testing.T) {
	helper := false
	for _, arg := range os.Args {
		if arg == "codex-usage-helper" {
			helper = true
			break
		}
	}
	if !helper {
		return
	}
	dec, enc := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	var request map[string]any
	if err := dec.Decode(&request); err != nil {
		os.Exit(2)
	}
	_ = enc.Encode(map[string]any{"id": 1, "result": map[string]any{}})
	if err := dec.Decode(&request); err != nil { // initialized notification
		os.Exit(2)
	}
	if err := dec.Decode(&request); err != nil { // rateLimits/read request
		os.Exit(2)
	}
	_ = enc.Encode(map[string]any{
		"id": 2,
		"result": map[string]any{"rateLimits": map[string]any{
			"primary":   map[string]any{"usedPercent": 41, "windowDurationMins": 300},
			"secondary": map[string]any{"usedPercent": 73, "windowDurationMins": 10080},
		}},
	})
	os.Exit(0)
}
