// Package usage reads the Claude plan usage-limit utilization that Claude
// Code's /usage screen reports: the rolling five-hour session window, the
// seven-day window, and the seven-day Opus window. It reuses the OAuth token
// Claude Code already stores on the machine, so ct never handles a password.
package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// ErrNoCredentials means Claude Code's OAuth credentials could not be found.
// Callers treat this as "the user isn't signed into Claude Code" and hide the
// feature rather than surfacing an error.
var ErrNoCredentials = errors.New("usage: no Claude Code credentials found")

// Window is one usage-limit window.
type Window struct {
	Utilization float64   // percent used, 0–100
	ResetsAt    time.Time // zero if unknown
}

// Snapshot is one fetch of the account's usage-limit state.
type Snapshot struct {
	FiveHour     *Window // nil when the endpoint omits it
	SevenDay     *Window
	SevenDayOpus *Window
	FetchedAt    time.Time
}

// Limit identifies which window is the binding constraint.
type Limit int

const (
	LimitNone Limit = iota
	LimitSession
	LimitWeek
	LimitOpus
)

// Binding returns the window with the highest utilization and which limit it
// is. Ties resolve to the shortest window (session before week before opus),
// since that is the one that frees up soonest. Returns (nil, LimitNone) when no
// window is present.
func (s Snapshot) Binding() (*Window, Limit) {
	var (
		best  *Window
		which = LimitNone
	)
	// Ordered shortest-window-first so equal utilizations keep the earlier,
	// sooner-to-reset limit.
	candidates := []struct {
		w     *Window
		limit Limit
	}{
		{s.FiveHour, LimitSession},
		{s.SevenDay, LimitWeek},
		{s.SevenDayOpus, LimitOpus},
	}
	for _, c := range candidates {
		if c.w == nil {
			continue
		}
		if best == nil || c.w.Utilization > best.Utilization {
			best, which = c.w, c.limit
		}
	}
	return best, which
}

// usageResponse mirrors the live shape of GET
// https://api.anthropic.com/api/oauth/usage (verified 2026-07). The payload
// carries many named windows, most null; ct only reads the three the /usage
// screen shows. Each present window is an object with a `utilization` percent
// (0–100) and an RFC3339 `resets_at`.
type usageResponse struct {
	FiveHour     *windowJSON `json:"five_hour"`
	SevenDay     *windowJSON `json:"seven_day"`
	SevenDayOpus *windowJSON `json:"seven_day_opus"`
}

type windowJSON struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

func (w *windowJSON) toWindow() *Window {
	if w == nil {
		return nil
	}
	out := &Window{Utilization: w.Utilization}
	// resets_at is RFC3339 with sub-second precision and a zone offset; leave
	// ResetsAt zero if it's missing or malformed rather than failing the fetch.
	if w.ResetsAt != "" {
		if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
			out.ResetsAt = t
		}
	}
	return out
}

const (
	usageURL    = "https://api.anthropic.com/api/oauth/usage"
	oauthBeta   = "oauth-2025-04-20"
	credService = "Claude Code-credentials"
)

// Fetch retrieves the current usage snapshot using the local Claude Code OAuth
// credentials. It is safe to call off the UI goroutine and honors ctx
// timeout/cancellation.
func Fetch(ctx context.Context) (Snapshot, error) {
	token, err := accessToken()
	if err != nil {
		return Snapshot{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return Snapshot{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", oauthBeta)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Snapshot{}, fmt.Errorf("usage: fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("usage: endpoint returned %s", resp.Status)
	}

	snap, err := parseUsage(resp.Body)
	if err != nil {
		return Snapshot{}, err
	}
	snap.FetchedAt = time.Now()
	return snap, nil
}

// parseUsage decodes a usage response body into a Snapshot. Split out from
// Fetch so tests can exercise the parser without a network round-trip.
func parseUsage(r io.Reader) (Snapshot, error) {
	var raw usageResponse
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return Snapshot{}, fmt.Errorf("usage: decode: %w", err)
	}
	return Snapshot{
		FiveHour:     raw.FiveHour.toWindow(),
		SevenDay:     raw.SevenDay.toWindow(),
		SevenDayOpus: raw.SevenDayOpus.toWindow(),
	}, nil
}

// credentialsJSON is the shape Claude Code writes both to the macOS Keychain
// and to ~/.claude/.credentials.json.
type credentialsJSON struct {
	ClaudeAiOauth struct {
		AccessToken string `json:"accessToken"`
	} `json:"claudeAiOauth"`
}

// accessToken discovers Claude Code's OAuth access token: the login Keychain on
// darwin, otherwise the credentials file that other platforms use.
func accessToken() (string, error) {
	if runtime.GOOS == "darwin" {
		if tok, err := keychainToken(); err == nil {
			return tok, nil
		}
		// Fall through to the file: some setups store credentials on disk even
		// on macOS.
	}
	return fileToken()
}

func keychainToken() (string, error) {
	out, err := exec.Command("security", "find-generic-password",
		"-s", credService, "-w").Output()
	if err != nil {
		return "", ErrNoCredentials
	}
	return tokenFromJSON(out)
}

func fileToken() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNoCredentials
		}
		return "", err
	}
	return tokenFromJSON(data)
}

func tokenFromJSON(data []byte) (string, error) {
	var creds credentialsJSON
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("usage: parsing credentials: %w", err)
	}
	if creds.ClaudeAiOauth.AccessToken == "" {
		return "", ErrNoCredentials
	}
	return creds.ClaudeAiOauth.AccessToken, nil
}
