package session

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
)

// visibleSleeper starts a quiet, on-screen session for exercising the repaint
// path. "sleep 60" produces no output of its own, so every dirty signal the
// test observes is one it sent via signalDirty.
func visibleSleeper(t *testing.T, m *Manager) *Session {
	t.Helper()
	s, err := Start(Terminal, "", t.TempDir(), []string{"sleep", "60"}, 120, 40, m.signalDirty)
	if err != nil {
		t.Fatal(err)
	}
	m.SetVisible(s)
	// Drop any stray startup signal so the measurement starts from quiet.
	select {
	case <-m.Dirty():
	default:
	}
	return s
}

// TestRepaintLeadingEdge proves the coalescing window never delays a lone
// signal that arrives after a quiet gap — the input-echo latency path.
func TestRepaintLeadingEdge(t *testing.T) {
	m := NewManager()
	defer m.CloseAll()
	s := visibleSleeper(t, m)

	// Prime lastRepaint with one repaint, then let the window fully lapse.
	m.signalDirty(s)
	m.WaitDirty()
	time.Sleep(3 * repaintWindow)

	// A solitary signal must be released essentially immediately.
	m.signalDirty(s)
	start := time.Now()
	m.WaitDirty()
	if elapsed := time.Since(start); elapsed >= repaintWindow {
		t.Fatalf("leading-edge repaint waited %v, want well under the window %v", elapsed, repaintWindow)
	}
}

// TestRepaintCoalescing proves a storm of rapid signals collapses into far
// fewer repaints than signals sent, and logs the before/after delivery counts
// (raw channel vs. WaitDirty) over an identical storm with a realistic render
// cost, so the win is measured rather than assumed.
func TestRepaintCoalescing(t *testing.T) {
	m := NewManager()
	defer m.CloseAll()
	s := visibleSleeper(t, m)

	const burst = 150 * time.Millisecond

	// A stand-in for the per-frame render cost: serialising a full styled vt
	// buffer, the exact work BenchmarkEmulatorRender measures.
	emu := vt.NewSafeEmulator(120, 40)
	line := strings.Repeat("\x1b[33mlorem\x1b[0m \x1b[1;34mipsum\x1b[0m dolor ", 12)
	for i := 0; i < 40; i++ {
		_, _ = emu.Write([]byte(line + "\r\n"))
	}
	render := func() { _ = emu.Render() }

	// before: the old path — consume the raw channel and render each token.
	before := stormDeliveries(m, s, burst, render, func() { <-m.Dirty() })
	// after: the coalescing path.
	after := stormDeliveries(m, s, burst, render, m.WaitDirty)

	t.Logf("write storm %v: before=%d repaints, after=%d repaints (%.1fx fewer)",
		burst, before, after, float64(before)/float64(max(1, after)))

	// The window caps sustained repaints near burst/repaintWindow (~12 here);
	// allow a generous ceiling to stay off wall-clock flake.
	if maxAfter := int(burst/repaintWindow) + 8; after > maxAfter {
		t.Fatalf("after=%d repaints over %v, want <= %d (window %v)", after, burst, maxAfter, repaintWindow)
	}
	if after >= before {
		t.Fatalf("coalescing did not reduce repaints: before=%d after=%d", before, after)
	}
	// The uncapped path renders far more; the ratio is ~68x in a normal run
	// and compresses under -race, so assert only a conservative multiple here.
	if before < after*2 {
		t.Fatalf("expected the uncapped path to render far more: before=%d after=%d", before, after)
	}
}

// stormDeliveries runs a tight signalDirty producer for d while a consumer
// pulls repaints via wait (rendering each), and returns how many repaints the
// consumer served.
func stormDeliveries(m *Manager, s *Session, d time.Duration, render, wait func()) int {
	// Start from quiet so a token left over from a previous run isn't counted.
	select {
	case <-m.Dirty():
	default:
	}

	var count int64
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		for {
			wait()
			render()
			atomic.AddInt64(&count, 1)
			select {
			case <-stop:
				return
			default:
			}
		}
	}()

	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		m.signalDirty(s)
	}
	// Let the trailing repaint land, then release the consumer's final wait.
	time.Sleep(2 * repaintWindow)
	close(stop)
	m.signalDirty(s)
	<-done

	return int(atomic.LoadInt64(&count))
}
