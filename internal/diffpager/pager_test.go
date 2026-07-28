package diffpager

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderSplitsTwoFileChunks proves the split-and-pipe round trip with cat
// as the formatter: the two `diff --git` headers become two File:true chunks
// whose content matches the input byte-for-byte.
func TestRenderSplitsTwoFileChunks(t *testing.T) {
	body := "diff --git a/foo b/foo\n+hello\ndiff --git a/bar b/bar\n+world\n"
	p := Pager{Command: "cat"}
	chunks, err := p.Render(t.TempDir(), body, 80)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := []Chunk{
		{Lines: []string{"diff --git a/foo b/foo", "+hello"}, File: true},
		{Lines: []string{"diff --git a/bar b/bar", "+world"}, File: true},
	}
	if !chunksEqual(chunks, want) {
		t.Fatalf("got %#v, want %#v", chunks, want)
	}
}

// TestRenderPreambleChunk proves text before the first `diff --git` header
// (git bodies don't normally have any, but losing it would be worse than one
// extra chunk) becomes a leading File:false chunk.
func TestRenderPreambleChunk(t *testing.T) {
	body := "commit abc123\nsome preamble\ndiff --git a/foo b/foo\n+hello\n"
	p := Pager{Command: "cat"}
	chunks, err := p.Render(t.TempDir(), body, 80)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := []Chunk{
		{Lines: []string{"commit abc123", "some preamble"}, File: false},
		{Lines: []string{"diff --git a/foo b/foo", "+hello"}, File: true},
	}
	if !chunksEqual(chunks, want) {
		t.Fatalf("got %#v, want %#v", chunks, want)
	}
}

// TestRenderANSIHeaderSplits proves the split still finds `diff --git`
// headers when a user's git diff args (e.g. --color=always) wrap them in real
// ANSI escapes; a naive strings.HasPrefix would collapse this into one chunk.
func TestRenderANSIHeaderSplits(t *testing.T) {
	body := "\x1b[1mdiff --git a/foo b/foo\x1b[0m\n\x1b[32m+hello\x1b[0m\n" +
		"\x1b[1mdiff --git a/bar b/bar\x1b[0m\n\x1b[32m+world\x1b[0m\n"
	p := Pager{Command: "cat"}
	chunks, err := p.Render(t.TempDir(), body, 80)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2: %#v", len(chunks), chunks)
	}
	for i, c := range chunks {
		if !c.File {
			t.Errorf("chunk %d: File = false, want true", i)
		}
	}
	if chunks[0].Lines[0] != "\x1b[1mdiff --git a/foo b/foo\x1b[0m" {
		t.Errorf("chunk 0 line 0 = %q, ANSI content was not preserved", chunks[0].Lines[0])
	}
}

// TestRenderWidthSubstitution proves {width} is substituted into argv
// (not just whole-argument matches) and that width <= 0 falls back to 80.
func TestRenderWidthSubstitution(t *testing.T) {
	p := Pager{
		Command: "sh",
		Args:    []string{"-c", `echo "$1"; cat`, "_", "--width={width}"},
	}
	body := "diff --git a/foo b/foo\n+hello\n"

	chunks, err := p.Render(t.TempDir(), body, 123)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := []string{"--width=123", "diff --git a/foo b/foo", "+hello"}
	if len(chunks) != 1 || !equalLines(chunks[0].Lines, want) {
		t.Fatalf("got %#v, want %#v", chunks, want)
	}

	for _, w := range []int{0, -5} {
		chunks, err = p.Render(t.TempDir(), body, w)
		if err != nil {
			t.Fatalf("Render(width=%d): %v", w, err)
		}
		if len(chunks) != 1 || len(chunks[0].Lines) == 0 || chunks[0].Lines[0] != "--width=80" {
			t.Fatalf("Render(width=%d): got %#v, want --width=80 fallback", w, chunks)
		}
	}
}

// TestRenderColumnsEnv proves COLUMNS reaches the child's environment when
// width > 0, and is absent when it is not.
func TestRenderColumnsEnv(t *testing.T) {
	p := Pager{Command: "sh", Args: []string{"-c", `echo "${COLUMNS:-unset}"`}}
	body := "diff --git a/foo b/foo\n+hello\n"

	chunks, err := p.Render(t.TempDir(), body, 100)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(chunks) != 1 || len(chunks[0].Lines) != 1 || chunks[0].Lines[0] != "100" {
		t.Fatalf("got %#v, want COLUMNS=100 to reach the child", chunks)
	}

	chunks, err = p.Render(t.TempDir(), body, 0)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(chunks) != 1 || len(chunks[0].Lines) != 1 || chunks[0].Lines[0] != "unset" {
		t.Fatalf("got %#v, want COLUMNS unset when width <= 0", chunks)
	}
}

// TestRenderOrderingConcurrent forces the worker pool to queue (more chunks
// than the 4-worker cap) and proves results are still reassembled in patch
// order, not completion order.
func TestRenderOrderingConcurrent(t *testing.T) {
	const n = 20
	var sb strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "diff --git a/file%d b/file%d\n+line%d\n", i, i, i)
	}
	p := Pager{Command: "sed", Args: []string{"-n", "1p"}}
	chunks, err := p.Render(t.TempDir(), sb.String(), 80)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(chunks) != n {
		t.Fatalf("got %d chunks, want %d", len(chunks), n)
	}
	for i, c := range chunks {
		want := fmt.Sprintf("diff --git a/file%d b/file%d", i, i)
		if len(c.Lines) != 1 || c.Lines[0] != want {
			t.Fatalf("chunk %d: got %#v, want [%q]", i, c.Lines, want)
		}
	}
}

// TestRenderFormatterError proves a non-zero exit makes Render return an
// error including the child's stderr, and nil chunks — never partial output.
func TestRenderFormatterError(t *testing.T) {
	p := Pager{Command: "sh", Args: []string{"-c", `echo "boom to stderr" >&2; exit 3`}}
	body := "diff --git a/foo b/foo\n+hello\n"
	chunks, err := p.Render(t.TempDir(), body, 80)
	if err == nil {
		t.Fatal("Render: got nil error, want failure")
	}
	if !strings.Contains(err.Error(), "boom to stderr") {
		t.Errorf("error = %q, want it to include child stderr", err.Error())
	}
	if chunks != nil {
		t.Errorf("chunks = %#v, want nil on error", chunks)
	}
}

// TestRenderNonexistentCommand proves an unresolvable command name is an
// error (not a panic or a hang) and returns nil chunks.
func TestRenderNonexistentCommand(t *testing.T) {
	p := Pager{Command: "ct-diffpager-nonexistent-command-xyz"}
	body := "diff --git a/foo b/foo\n+hello\n"
	chunks, err := p.Render(t.TempDir(), body, 80)
	if err == nil {
		t.Fatal("Render: got nil error, want failure")
	}
	if chunks != nil {
		t.Errorf("chunks = %#v, want nil on error", chunks)
	}
}

// TestRenderMaxChunksExceeded proves a patch with more than maxChunks files
// errors out before spawning any process, rather than spawning hundreds.
func TestRenderMaxChunksExceeded(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < maxChunks+1; i++ {
		fmt.Fprintf(&sb, "diff --git a/f%d b/f%d\n+x\n", i, i)
	}
	p := Pager{Command: "cat"}
	chunks, err := p.Render(t.TempDir(), sb.String(), 80)
	if err == nil {
		t.Fatal("Render: got nil error, want the chunk-limit error")
	}
	if chunks != nil {
		t.Errorf("chunks = %#v, want nil on error", chunks)
	}
}

// TestRenderEmptyAndDisabled proves an empty/whitespace-only body and a
// disabled (Command == "") pager both return nil, nil, and that the empty
// body case never spawns a process.
func TestRenderEmptyAndDisabled(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	p := Pager{Command: "sh", Args: []string{"-c", "touch " + marker}}

	for _, body := range []string{"", "   \n\t\n  "} {
		chunks, err := p.Render(dir, body, 80)
		if err != nil {
			t.Fatalf("Render(%q): %v", body, err)
		}
		if chunks != nil {
			t.Errorf("Render(%q): chunks = %#v, want nil", body, chunks)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker file exists, want no process spawned for empty body")
	}

	disabled := Pager{}
	chunks, err := disabled.Render(dir, "diff --git a/foo b/foo\n+hello\n", 80)
	if err != nil {
		t.Fatalf("Render with disabled pager: %v", err)
	}
	if chunks != nil {
		t.Errorf("chunks = %#v, want nil for a disabled pager", chunks)
	}
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func chunksEqual(a, b []Chunk) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].File != b[i].File || !equalLines(a[i].Lines, b[i].Lines) {
			return false
		}
	}
	return true
}
