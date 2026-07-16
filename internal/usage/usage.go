// Package usage reads plan usage-limit utilization from the local Claude Code
// and Codex sign-ins. It never handles a password: Claude reuses its stored
// OAuth token, while Codex is queried through its read-only App Server API.
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
	"strings"
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
	// Named contains provider-defined windows. Claude leaves this nil and uses
	// the three stable fields above; Codex supplies primary/secondary windows
	// whose duration can vary by plan.
	Named     []NamedWindow
	FetchedAt time.Time
}

// NamedWindow gives a usage window its panel and compact bar labels. Session
// windows use countdown reset text; longer windows use a weekday.
type NamedWindow struct {
	Label      string
	ShortLabel string
	Session    bool
	Window     *Window
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

// Windows returns the snapshot's display windows in shortest-to-longest
// order. Provider-defined windows take precedence over Claude's fixed shape.
func (s Snapshot) Windows() []NamedWindow {
	if s.Named != nil {
		return s.Named
	}
	return []NamedWindow{
		{Label: "session", Session: true, Window: s.FiveHour},
		{Label: "week", ShortLabel: "wk", Window: s.SevenDay},
		{Label: "opus", ShortLabel: "opus", Window: s.SevenDayOpus},
	}
}

// BindingWindow returns the most-utilized display window. Ties retain the
// earlier (normally shorter) window.
func (s Snapshot) BindingWindow() (NamedWindow, bool) {
	var best NamedWindow
	found := false
	for _, candidate := range s.Windows() {
		if candidate.Window == nil {
			continue
		}
		if !found || candidate.Window.Utilization > best.Window.Utilization {
			best, found = candidate, true
		}
	}
	return best, found
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

// FetchCodex retrieves the current Codex rate-limit snapshot through a short-
// lived App Server process. command and args are the configured Codex provider
// command; the app-server arguments are appended in the same way as interactive
// Codex sessions.
func FetchCodex(ctx context.Context, command string, args []string) (Snapshot, error) {
	if command == "" {
		command = "codex"
	}
	argv := append([]string(nil), args...)
	argv = append(argv, "app-server", "--listen", "stdio://")
	cmd := exec.CommandContext(ctx, command, argv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Snapshot{}, fmt.Errorf("usage: open Codex stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Snapshot{}, fmt.Errorf("usage: open Codex stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return Snapshot{}, fmt.Errorf("usage: start Codex App Server: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	enc, dec := json.NewEncoder(stdin), json.NewDecoder(stdout)
	if err := enc.Encode(map[string]any{
		"method": "initialize", "id": 1,
		"params": map[string]any{"clientInfo": map[string]string{
			"name": "caretaker", "title": "Caretaker", "version": "dev",
		}},
	}); err != nil {
		return Snapshot{}, fmt.Errorf("usage: initialize Codex App Server: %w", err)
	}
	if _, err := readCodexResponse(dec, 1); err != nil {
		return Snapshot{}, err
	}
	if err := enc.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return Snapshot{}, fmt.Errorf("usage: initialize Codex client: %w", err)
	}
	if err := enc.Encode(map[string]any{"method": "account/rateLimits/read", "id": 2, "params": nil}); err != nil {
		return Snapshot{}, fmt.Errorf("usage: request Codex rate limits: %w", err)
	}
	result, err := readCodexResponse(dec, 2)
	if err != nil {
		return Snapshot{}, err
	}
	snap, err := parseCodexUsage(result)
	if err != nil {
		return Snapshot{}, err
	}
	snap.FetchedAt = time.Now()
	return snap, nil
}

type codexRPCMessage struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func readCodexResponse(dec *json.Decoder, id int) (json.RawMessage, error) {
	want := fmt.Sprint(id)
	for {
		var msg codexRPCMessage
		if err := dec.Decode(&msg); err != nil {
			return nil, fmt.Errorf("usage: read Codex App Server response: %w", err)
		}
		if string(msg.ID) != want {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("usage: Codex App Server rejected request (%d): %s", msg.Error.Code, msg.Error.Message)
		}
		if msg.Result == nil {
			return nil, errors.New("usage: Codex App Server response has no result")
		}
		return msg.Result, nil
	}
}

type codexRateLimitResponse struct {
	RateLimits          codexRateLimitSnapshot            `json:"rateLimits"`
	RateLimitsByLimitID map[string]codexRateLimitSnapshot `json:"rateLimitsByLimitId"`
}

type codexRateLimitSnapshot struct {
	Primary   *codexRateLimitWindow `json:"primary"`
	Secondary *codexRateLimitWindow `json:"secondary"`
}

type codexRateLimitWindow struct {
	UsedPercent       float64 `json:"usedPercent"`
	WindowDurationMin *int64  `json:"windowDurationMins"`
	ResetsAt          *int64  `json:"resetsAt"`
}

func parseCodexUsage(data []byte) (Snapshot, error) {
	var raw codexRateLimitResponse
	if err := json.Unmarshal(data, &raw); err != nil {
		return Snapshot{}, fmt.Errorf("usage: decode Codex rate limits: %w", err)
	}
	limits := raw.RateLimits
	if named, ok := raw.RateLimitsByLimitID["codex"]; ok {
		limits = named
	}

	var windows []NamedWindow
	add := func(fallback string, raw *codexRateLimitWindow) {
		if raw == nil {
			return
		}
		label, short, session := codexWindowLabel(fallback, raw.WindowDurationMin)
		window := &Window{Utilization: raw.UsedPercent}
		if raw.ResetsAt != nil {
			window.ResetsAt = time.Unix(*raw.ResetsAt, 0)
		}
		windows = append(windows, NamedWindow{
			Label: label, ShortLabel: short, Session: session, Window: window,
		})
	}
	add("primary", limits.Primary)
	add("secondary", limits.Secondary)
	return Snapshot{Named: windows}, nil
}

func codexWindowLabel(fallback string, minutes *int64) (label, short string, session bool) {
	if minutes == nil || *minutes <= 0 {
		return fallback, fallback, false
	}
	mins := *minutes
	switch {
	case mins < 24*60:
		return "session", "", true
	case mins == 7*24*60:
		return "week", "wk", false
	case mins%(24*60) == 0:
		label = fmt.Sprintf("%dd", mins/(24*60))
	case mins%60 == 0:
		label = fmt.Sprintf("%dh", mins/60)
	default:
		label = fmt.Sprintf("%dm", mins)
	}
	return label, strings.ToLower(label), false
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
