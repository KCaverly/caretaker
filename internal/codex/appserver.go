// Package codex observes a pane-local Codex App Server.
//
// The observer performs only the App Server initialization handshake and then
// remains read-only. In particular, it never starts threads, subscribes to
// threads, or responds to approval and user-input requests; the interactive
// Codex client connected to RemoteURL remains the sole owner of those actions.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/KCaverly/caretaker/internal/agent"
)

const (
	defaultCommand        = "codex"
	defaultStartupTimeout = 5 * time.Second
	initializeRequestID   = 1
)

// Config controls the App Server process owned by an Observer.
type Config struct {
	// Command is the Codex executable. It defaults to "codex".
	Command string
	// Args are placed before "app-server --listen unix://...". This allows a
	// configured Codex command to retain its existing base arguments.
	Args []string
	// Dir is the process working directory. Empty means the current directory.
	Dir string
	// Env entries are added to the inherited environment, replacing entries
	// with the same key.
	Env []string
	// StartupTimeout bounds socket creation, connection, and initialization.
	// It defaults to five seconds. A sooner deadline on the Start context wins.
	StartupTimeout time.Duration
	// ClientVersion is reported in initialize.clientInfo. It defaults to "dev".
	ClientVersion string
}

// EventKind identifies a normalized App Server notification.
type EventKind = agent.EventKind

const (
	ThreadStarted       = agent.ThreadStarted
	ThreadStatusChanged = agent.ThreadStatusChanged
	TurnStarted         = agent.TurnStarted
	TurnCompleted       = agent.TurnCompleted
	Error               = agent.Error
	Disconnected        = agent.Disconnected
)

// Event is the provider-neutral subset of App Server state caretaker needs.
// Unknown JSON fields and unknown notification methods are ignored.
type Event = agent.Event

// Observer owns one Codex App Server and passively observes its notifications.
// RemoteURL can be passed to the interactive Codex client for the same pane.
type Observer struct {
	RemoteURL string
	Events    <-chan Event

	eventCh chan Event
	cmd     *exec.Cmd
	connMu  sync.Mutex
	conn    *websocket.Conn

	socketDir string
	procDone  chan struct{}
	readDone  chan struct{}

	procMu  sync.Mutex
	procErr error

	closing      atomic.Bool
	cleanupOnce  sync.Once
	eventsOnce   sync.Once
	processAlive atomic.Bool
}

// Remote returns the Unix-socket URL for the interactive Codex client.
func (o *Observer) Remote() string { return o.RemoteURL }

// EventStream returns the passive provider lifecycle stream.
func (o *Observer) EventStream() <-chan agent.Event { return o.Events }

// Start launches and initializes a pane-local Codex App Server.
func Start(ctx context.Context, cfg Config) (*Observer, error) {
	if cfg.Command == "" {
		cfg.Command = defaultCommand
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = defaultStartupTimeout
	}
	if cfg.ClientVersion == "" {
		cfg.ClientVersion = "dev"
	}

	socketDir, err := os.MkdirTemp(os.TempDir(), "ct-cdx-")
	if err != nil {
		return nil, fmt.Errorf("create Codex socket directory: %w", err)
	}
	// MkdirTemp currently creates mode 0700, but make the privacy requirement
	// explicit rather than relying on that implementation detail.
	if err := os.Chmod(socketDir, 0o700); err != nil {
		_ = os.RemoveAll(socketDir)
		return nil, fmt.Errorf("secure Codex socket directory: %w", err)
	}

	socketPath := filepath.Join(socketDir, "s")
	remoteURL := "unix://" + socketPath
	args := append([]string(nil), cfg.Args...)
	args = append(args, "app-server", "--listen", remoteURL)
	cmd := exec.Command(cfg.Command, args...)
	cmd.Dir = cfg.Dir
	cmd.Env = mergeEnv(os.Environ(), cfg.Env)

	eventCh := make(chan Event, 64)
	o := &Observer{
		RemoteURL: remoteURL,
		Events:    eventCh,
		eventCh:   eventCh,
		cmd:       cmd,
		socketDir: socketDir,
		procDone:  make(chan struct{}),
		readDone:  make(chan struct{}),
	}

	if err := cmd.Start(); err != nil {
		o.cleanup()
		return nil, fmt.Errorf("start Codex App Server: %w", err)
	}
	o.processAlive.Store(true)
	go o.waitProcess()

	startupCtx, cancel := context.WithTimeout(ctx, cfg.StartupTimeout)
	defer cancel()

	conn, err := o.connect(startupCtx, socketPath)
	if err != nil {
		o.abortStartup()
		return nil, err
	}
	o.setConn(conn)

	if err := initialize(startupCtx, conn, cfg.ClientVersion); err != nil {
		o.abortStartup()
		return nil, fmt.Errorf("initialize Codex App Server: %w", err)
	}

	go o.readLoop()
	return o, nil
}

// New is an alias for Start.
func New(ctx context.Context, cfg Config) (*Observer, error) { return Start(ctx, cfg) }

func (o *Observer) connect(ctx context.Context, socketPath string) (*websocket.Conn, error) {
	dialer := websocket.Dialer{
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(socketPath); err == nil {
			conn, resp, dialErr := dialer.DialContext(ctx, "ws://localhost/", nil)
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if dialErr == nil {
				return conn, nil
			}
			// Socket creation can precede the accept loop. Retry until the
			// process exits or the startup context expires.
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for Codex App Server socket: %w", ctx.Err())
		case <-o.procDone:
			return nil, o.startupProcessError()
		case <-ticker.C:
		}
	}
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func initialize(ctx context.Context, conn *websocket.Conn, version string) error {
	deadline, ok := ctx.Deadline()
	if ok {
		if err := conn.SetWriteDeadline(deadline); err != nil {
			return err
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return err
		}
	}

	request := struct {
		Method string `json:"method"`
		ID     int    `json:"id"`
		Params struct {
			ClientInfo struct {
				Name    string `json:"name"`
				Title   string `json:"title"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		} `json:"params"`
	}{Method: "initialize", ID: initializeRequestID}
	request.Params.ClientInfo.Name = "caretaker"
	request.Params.ClientInfo.Title = "Caretaker"
	request.Params.ClientInfo.Version = version
	if err := conn.WriteJSON(request); err != nil {
		return fmt.Errorf("send initialize request: %w", err)
	}

	var response rpcMessage
	if err := conn.ReadJSON(&response); err != nil {
		return fmt.Errorf("read initialize response: %w", err)
	}
	if string(response.ID) != fmt.Sprint(initializeRequestID) {
		return fmt.Errorf("initialize response id %q does not match request id %d", response.ID, initializeRequestID)
	}
	if response.Error != nil {
		return fmt.Errorf("initialize rejected (%d): %s", response.Error.Code, response.Error.Message)
	}
	if response.Result == nil {
		return errors.New("initialize response has no result")
	}

	initialized := struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}{Method: "initialized", Params: map[string]any{}}
	if err := conn.WriteJSON(initialized); err != nil {
		return fmt.Errorf("send initialized notification: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})
	_ = conn.SetReadDeadline(time.Time{})
	return nil
}

func (o *Observer) readLoop() {
	defer func() {
		o.cleanup()
		o.eventsOnce.Do(func() { close(o.eventCh) })
		close(o.readDone)
	}()

	for {
		var msg rpcMessage
		if err := o.conn.ReadJSON(&msg); err != nil {
			if !o.closing.Load() {
				o.emit(Event{Kind: Disconnected, Message: err.Error(), Err: err})
			}
			return
		}
		// Messages with IDs are server requests or responses. The observer is
		// deliberately passive and never responds to them.
		if len(msg.ID) != 0 && string(msg.ID) != "null" {
			continue
		}
		if event, ok := parseEvent(msg.Method, msg.Params); ok {
			o.emit(event)
		}
	}
}

func parseEvent(method string, raw json.RawMessage) (Event, bool) {
	switch method {
	case "thread/started":
		var params struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return parseError(method, err), true
		}
		return Event{Kind: ThreadStarted, ThreadID: params.Thread.ID}, true

	case "thread/status/changed":
		var params struct {
			ThreadID string          `json:"threadId"`
			Status   json.RawMessage `json:"status"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return parseError(method, err), true
		}
		status, approval, input, err := parseThreadStatus(params.Status)
		if err != nil {
			return parseError(method, err), true
		}
		return Event{
			Kind:               ThreadStatusChanged,
			ThreadID:           params.ThreadID,
			Status:             status,
			WaitingOnApproval:  approval,
			WaitingOnUserInput: input,
		}, true

	case "turn/started", "turn/completed":
		var params struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turn"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return parseError(method, err), true
		}
		kind := TurnStarted
		if method == "turn/completed" {
			kind = TurnCompleted
		}
		return Event{Kind: kind, ThreadID: params.ThreadID, TurnID: params.Turn.ID, Status: params.Turn.Status}, true

	case "error":
		var params struct {
			ThreadID string          `json:"threadId"`
			TurnID   string          `json:"turnId"`
			Error    json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return parseError(method, err), true
		}
		message := errorMessage(params.Error)
		return Event{
			Kind:     Error,
			ThreadID: params.ThreadID,
			TurnID:   params.TurnID,
			Message:  message,
			Err:      errors.New(message),
		}, true

	default:
		return Event{}, false
	}
}

func parseThreadStatus(raw json.RawMessage) (status string, approval, input bool, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false, false, errors.New("missing thread status")
	}
	var scalar string
	if err := json.Unmarshal(raw, &scalar); err == nil {
		return scalar, false, false, nil
	}
	var value struct {
		Type        string   `json:"type"`
		ActiveFlags []string `json:"activeFlags"`
		Flags       []string `json:"flags"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false, false, fmt.Errorf("decode thread status: %w", err)
	}
	for _, flag := range append(value.ActiveFlags, value.Flags...) {
		switch flag {
		case "waitingOnApproval":
			approval = true
		case "waitingOnUserInput":
			input = true
		}
	}
	return value.Type, approval, input, nil
}

func errorMessage(raw json.RawMessage) string {
	var message string
	if err := json.Unmarshal(raw, &message); err == nil && message != "" {
		return message
	}
	var value struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &value); err == nil && value.Message != "" {
		return value.Message
	}
	return "Codex App Server error"
}

func parseError(method string, err error) Event {
	wrapped := fmt.Errorf("decode %s notification: %w", method, err)
	return Event{Kind: Error, Message: wrapped.Error(), Err: wrapped}
}

func (o *Observer) emit(event Event) {
	select {
	case o.eventCh <- event:
	default:
		// Observation must never block the App Server connection. The buffer is
		// intentionally generous for the small lifecycle-only event stream.
	}
}

func (o *Observer) waitProcess() {
	err := o.cmd.Wait()
	o.procMu.Lock()
	o.procErr = err
	o.procMu.Unlock()
	o.processAlive.Store(false)
	close(o.procDone)
	o.closeConn()
}

func (o *Observer) startupProcessError() error {
	o.procMu.Lock()
	err := o.procErr
	o.procMu.Unlock()
	if err == nil {
		return errors.New("Codex App Server exited during startup")
	}
	return fmt.Errorf("Codex App Server exited during startup: %w", err)
}

func (o *Observer) abortStartup() {
	o.closing.Store(true)
	o.cleanup()
	<-o.procDone
}

func (o *Observer) cleanup() {
	o.cleanupOnce.Do(func() {
		o.closeConn()
		if o.processAlive.Load() && o.cmd.Process != nil {
			_ = o.cmd.Process.Kill()
		}
		_ = os.RemoveAll(o.socketDir)
	})
}

func (o *Observer) setConn(conn *websocket.Conn) {
	o.connMu.Lock()
	o.conn = conn
	processAlive := o.processAlive.Load()
	o.connMu.Unlock()
	// Cover the narrow race where the process exits after a successful dial
	// but before Start publishes the connection.
	if !processAlive {
		_ = conn.Close()
	}
}

func (o *Observer) closeConn() {
	o.connMu.Lock()
	conn := o.conn
	o.connMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// Close stops observation and its App Server process, removes the private
// socket directory, and closes Events. It is safe to call more than once.
func (o *Observer) Close() error {
	if o == nil {
		return nil
	}
	o.closing.Store(true)
	o.cleanup()
	<-o.readDone
	<-o.procDone
	return nil
}

func mergeEnv(base, additions []string) []string {
	env := append([]string(nil), base...)
	for _, addition := range additions {
		key, _, ok := strings.Cut(addition, "=")
		if !ok || key == "" {
			env = append(env, addition)
			continue
		}
		prefix := key + "="
		filtered := env[:0]
		for _, existing := range env {
			if !strings.HasPrefix(existing, prefix) {
				filtered = append(filtered, existing)
			}
		}
		env = append(filtered, addition)
	}
	return env
}
