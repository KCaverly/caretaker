package session

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
)

// BenchmarkEmulatorRender measures the cost of serialising a full emulator
// screen to a styled string — the work every repaint pays per visible session.
// This is what makes dropping repaints for invisible sessions worthwhile: the
// vt layer re-renders the whole buffer (w×h cells) on every call, regardless
// of how much changed.
func BenchmarkEmulatorRender(b *testing.B) {
	for _, size := range []struct{ w, h int }{{80, 24}, {120, 40}, {200, 50}} {
		b.Run(fmt.Sprintf("%dx%d", size.w, size.h), func(b *testing.B) {
			emu := vt.NewSafeEmulator(size.w, size.h)
			// Fill the screen with styled text so cells carry SGR attributes,
			// approximating a busy TUI program (editor/agent) rather than
			// blank cells.
			line := strings.Repeat("\x1b[33mlorem\x1b[0m \x1b[1;34mipsum\x1b[0m dolor ", 12)
			for i := 0; i < size.h; i++ {
				_, _ = emu.Write([]byte(line + "\r\n"))
			}
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = emu.Render()
			}
		})
	}
}
