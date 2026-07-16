package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	fakeProcessEnv = "CARETAKER_CODEX_FAKE_PROCESS"
	fakeModeEnv    = "CARETAKER_CODEX_FAKE_MODE"
	fakeRecordEnv  = "CARETAKER_CODEX_FAKE_RECORD"
)

func TestObserverHandshakeAndEvents(t *testing.T) {
	useSocketTemp(t)
	recordPath := filepath.Join(t.TempDir(), "handshake.json")
	o, err := Start(context.Background(), fakeConfig("events-disconnect", recordPath))
	if err != nil {
		detail, _ := os.ReadFile(recordPath)
		t.Fatalf("Start: %v (fake record: %s)", err, detail)
	}
	t.Cleanup(func() { _ = o.Close() })

	if !strings.HasPrefix(o.RemoteURL, "unix://"+os.TempDir()+string(os.PathSeparator)) {
		t.Fatalf("RemoteURL = %q, want private socket beneath %q", o.RemoteURL, os.TempDir())
	}
	socketPath := strings.TrimPrefix(o.RemoteURL, "unix://")
	info, err := os.Stat(filepath.Dir(socketPath))
	if err != nil {
		t.Fatalf("stat socket directory: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("socket directory mode = %o, want 700", info.Mode().Perm())
	}

	var events []Event
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-o.Events:
			if !ok {
				goto eventsClosed
			}
			events = append(events, event)
		case <-timer.C:
			t.Fatal("timed out waiting for observer events to close")
		}
	}

eventsClosed:
	if len(events) != 9 {
		t.Fatalf("got %d events, want 9: %#v", len(events), events)
	}
	assertEvent(t, events[0], Event{Kind: ThreadStarted, ThreadID: "thread-1"})
	assertEvent(t, events[1], Event{Kind: ThreadStatusChanged, ThreadID: "thread-1", Status: "idle"})
	assertEvent(t, events[2], Event{
		Kind:               ThreadStatusChanged,
		ThreadID:           "thread-1",
		Status:             "active",
		WaitingOnApproval:  true,
		WaitingOnUserInput: true,
	})
	assertEvent(t, events[3], Event{Kind: ThreadStatusChanged, ThreadID: "thread-1", Status: "systemError"})
	assertEvent(t, events[4], Event{Kind: ThreadStatusChanged, ThreadID: "thread-1", Status: "notLoaded"})
	assertEvent(t, events[5], Event{Kind: TurnStarted, ThreadID: "thread-1", TurnID: "turn-1", Status: "inProgress"})
	assertEvent(t, events[6], Event{Kind: TurnCompleted, ThreadID: "thread-1", TurnID: "turn-1", Status: "failed"})
	if events[7].Kind != Error || events[7].ThreadID != "thread-1" || events[7].TurnID != "turn-1" || events[7].Message != "model overloaded" || events[7].Err == nil {
		t.Fatalf("error event = %#v", events[7])
	}
	if events[8].Kind != Disconnected || events[8].Err == nil {
		t.Fatalf("disconnect event = %#v", events[8])
	}

	var record fakeHandshakeRecord
	readJSONFile(t, recordPath, &record)
	if record.Initialize.Method != "initialize" || record.Initialize.ID != initializeRequestID {
		t.Fatalf("initialize envelope = %#v", record.Initialize)
	}
	if record.Initialize.Params.ClientInfo.Name != "caretaker" || record.Initialize.Params.ClientInfo.Title != "Caretaker" || record.Initialize.Params.ClientInfo.Version != "test-version" {
		t.Fatalf("clientInfo = %#v", record.Initialize.Params.ClientInfo)
	}
	if record.Initialized.Method != "initialized" || record.Initialized.Params == nil {
		t.Fatalf("initialized notification = %#v", record.Initialized)
	}
	if len(record.Args) < 4 || record.Args[len(record.Args)-4] != "base-marker" || record.Args[len(record.Args)-3] != "app-server" || record.Args[len(record.Args)-2] != "--listen" || record.Args[len(record.Args)-1] != o.RemoteURL {
		t.Fatalf("fake process args = %#v", record.Args)
	}

	if _, err := os.Stat(filepath.Dir(socketPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket directory remains after disconnect: %v", err)
	}
}

func TestObserverCloseIsIdempotentAndCleansUp(t *testing.T) {
	useSocketTemp(t)
	recordPath := filepath.Join(t.TempDir(), "handshake.json")
	o, err := Start(context.Background(), fakeConfig("hold", recordPath))
	if err != nil {
		detail, _ := os.ReadFile(recordPath)
		t.Fatalf("Start: %v (fake record: %s)", err, detail)
	}
	socketDir := filepath.Dir(strings.TrimPrefix(o.RemoteURL, "unix://"))

	if err := o.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := o.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, ok := <-o.Events; ok {
		t.Fatal("Events remains open after Close")
	}
	if _, err := os.Stat(socketDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket directory remains after Close: %v", err)
	}
}

func TestObserverReportsStartupProcessFailure(t *testing.T) {
	useSocketTemp(t)
	_, err := Start(context.Background(), fakeConfig("exit", filepath.Join(t.TempDir(), "args.json")))
	if err == nil || !strings.Contains(err.Error(), "exited during startup") {
		t.Fatalf("Start error = %v, want process-exit error", err)
	}
}

func TestObserverStartupTimeoutKillsProcessAndRemovesSocketDirectory(t *testing.T) {
	useSocketTemp(t)
	recordPath := filepath.Join(t.TempDir(), "args.json")
	cfg := fakeConfig("stall", recordPath)
	cfg.StartupTimeout = 100 * time.Millisecond
	_, err := Start(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("Start error = %v, want timeout", err)
	}

	var record struct {
		RemoteURL string `json:"remoteURL"`
	}
	readJSONFile(t, recordPath, &record)
	socketDir := filepath.Dir(strings.TrimPrefix(record.RemoteURL, "unix://"))
	if _, err := os.Stat(socketDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket directory remains after timeout: %v", err)
	}
}

func TestObserverRejectsMismatchedInitializeResponse(t *testing.T) {
	useSocketTemp(t)
	_, err := Start(context.Background(), fakeConfig("mismatch", filepath.Join(t.TempDir(), "handshake.json")))
	if err == nil || !strings.Contains(err.Error(), "does not match request id") {
		t.Fatalf("Start error = %v, want mismatched response error", err)
	}
}

func assertEvent(t *testing.T, got, want Event) {
	t.Helper()
	if got.Kind != want.Kind || got.ThreadID != want.ThreadID || got.TurnID != want.TurnID || got.Status != want.Status || got.WaitingOnApproval != want.WaitingOnApproval || got.WaitingOnUserInput != want.WaitingOnUserInput {
		t.Fatalf("event = %#v, want %#v", got, want)
	}
}

func fakeConfig(mode, recordPath string) Config {
	return Config{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestCodexFakeProcess", "--", "base-marker"},
		Env: []string{
			fakeProcessEnv + "=1",
			fakeModeEnv + "=" + mode,
			fakeRecordEnv + "=" + recordPath,
		},
		StartupTimeout: 2 * time.Second,
		ClientVersion:  "test-version",
	}
}

func useSocketTemp(t *testing.T) {
	t.Helper()
	// The managed test sandbox permits Unix socket creation under /tmp but not
	// beneath macOS's usual /var/folders TMPDIR. Production still uses the
	// caller's ordinary os.TempDir value.
	t.Setenv("TMPDIR", "/tmp")
}

type fakeHandshakeRecord struct {
	RemoteURL  string   `json:"remoteURL"`
	Args       []string `json:"args"`
	Initialize struct {
		Method string `json:"method"`
		ID     int    `json:"id"`
		Params struct {
			ClientInfo struct {
				Name    string `json:"name"`
				Title   string `json:"title"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		} `json:"params"`
	} `json:"initialize"`
	Initialized struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	} `json:"initialized"`
}

func readJSONFile(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

// TestCodexFakeProcess runs in a child copy of the test binary. It is a real
// WebSocket server over a Unix socket, but never executes Codex or opens a
// network socket.
func TestCodexFakeProcess(t *testing.T) {
	if os.Getenv(fakeProcessEnv) != "1" {
		return
	}

	mode := os.Getenv(fakeModeEnv)
	recordPath := os.Getenv(fakeRecordEnv)
	remoteURL := listenArg(os.Args)
	initialRecord := struct {
		RemoteURL string   `json:"remoteURL"`
		Args      []string `json:"args"`
	}{RemoteURL: remoteURL, Args: append([]string(nil), os.Args[1:]...)}
	writeChildJSON(recordPath, initialRecord)

	if mode == "exit" {
		os.Exit(23)
	}
	if mode == "stall" {
		for {
			time.Sleep(time.Hour)
		}
	}

	socketPath := strings.TrimPrefix(remoteURL, "unix://")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		writeChildJSON(recordPath, map[string]any{"remoteURL": remoteURL, "listenError": err.Error(), "args": os.Args[1:]})
		os.Exit(24)
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var initializeMessage json.RawMessage
		if _, data, err := conn.ReadMessage(); err != nil {
			return
		} else {
			initializeMessage = append(initializeMessage, data...)
		}
		var initializeEnvelope struct {
			ID int `json:"id"`
		}
		_ = json.Unmarshal(initializeMessage, &initializeEnvelope)
		responseID := initializeEnvelope.ID
		if mode == "mismatch" {
			responseID++
		}
		_ = conn.WriteJSON(map[string]any{"id": responseID, "result": map[string]any{"userAgent": "fake"}})
		if mode == "mismatch" {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}

		var initializedMessage json.RawMessage
		if _, data, err := conn.ReadMessage(); err != nil {
			return
		} else {
			initializedMessage = append(initializedMessage, data...)
		}
		record := struct {
			RemoteURL   string          `json:"remoteURL"`
			Args        []string        `json:"args"`
			Initialize  json.RawMessage `json:"initialize"`
			Initialized json.RawMessage `json:"initialized"`
		}{remoteURL, append([]string(nil), os.Args[1:]...), initializeMessage, initializedMessage}
		writeChildJSON(recordPath, record)

		if mode == "events-disconnect" {
			writeFakeEvents(conn)
			_ = conn.Close()
			_ = listener.Close()
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	_ = http.Serve(listener, mux)
}

func listenArg(args []string) string {
	for i := range args {
		if args[i] == "--listen" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func writeChildJSON(path string, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		os.Exit(25)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		os.Exit(26)
	}
}

func writeFakeEvents(conn *websocket.Conn) {
	events := []any{
		map[string]any{"method": "thread/started", "params": map[string]any{"thread": map[string]any{"id": "thread-1", "unknown": true}}},
		map[string]any{"method": "unknown/future/event", "params": map[string]any{"value": 1}},
		map[string]any{"method": "thread/status/changed", "params": map[string]any{"threadId": "thread-1", "status": map[string]any{"type": "idle"}}},
		map[string]any{"method": "thread/status/changed", "params": map[string]any{"threadId": "thread-1", "status": map[string]any{"type": "active", "activeFlags": []string{"waitingOnApproval", "waitingOnUserInput", "futureFlag"}}}},
		map[string]any{"method": "thread/status/changed", "params": map[string]any{"threadId": "thread-1", "status": map[string]any{"type": "systemError"}}},
		map[string]any{"method": "thread/status/changed", "params": map[string]any{"threadId": "thread-1", "status": map[string]any{"type": "notLoaded"}}},
		map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "inProgress"}}},
		map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "failed"}}},
		map[string]any{"method": "error", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "error": map[string]any{"message": "model overloaded"}}},
		// A server request is intentionally ignored and never answered.
		map[string]any{"id": 99, "method": "item/commandExecution/requestApproval", "params": map[string]any{}},
	}
	for _, event := range events {
		_ = conn.WriteJSON(event)
	}
}
