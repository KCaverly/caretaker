// Package diffpager pipes unified-diff bodies through an external formatter.
package diffpager

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// Pager hands a patch's line-by-line styling to an external command (delta,
// diff-so-fancy, bat, ...) while ct keeps ownership of scrolling and
// keybindings. A zero-value Pager is inert: see Enabled.
type Pager struct {
	Command string
	Args    []string
}

// Enabled reports whether a formatter is configured.
func (p Pager) Enabled() bool {
	return p.Command != ""
}

// Chunk is one rendered span of the patch. File marks the chunks that began at
// a `diff --git` header, which is what lets the caller keep its file-jump index.
type Chunk struct {
	Lines []string
	File  bool
}

// maxChunks bounds how many per-file pieces a single Render call will spawn a
// process for. Past this the cost of spawning that many formatter processes on
// the fetch path outweighs what the formatting buys; the caller's fallback is
// to re-render with ct's own built-in styling instead.
const maxChunks = 256

// renderTimeout bounds the whole Render call, mirroring repo.gitTimeout: a
// formatter that hangs — waiting on a pager it thinks it owns, a dead pipe,
// whatever — must not strand the fetch goroutine forever.
const renderTimeout = 30 * time.Second

// maxWorkers caps how many formatter processes run at once. A 40-file diff
// spawning 40 processes serially would cost 40x a single process's latency on
// the fetch path, but spawning them all at once would oversubscribe a small
// machine, so the pool is capped and further limited by GOMAXPROCS.
const maxWorkers = 4

// rawChunk is one pre-formatting span of the patch, produced by splitChunks.
type rawChunk struct {
	lines []string
	file  bool
}

// Render splits body into per-file chunks, pipes each through the formatter,
// and returns them in patch order.
func (p Pager) Render(dir, body string, width int) ([]Chunk, error) {
	// Callers are expected to check Enabled first; this guard just keeps a
	// misuse cheap (returning nothing) rather than spawning a "" command.
	if !p.Enabled() {
		return nil, nil
	}
	if strings.TrimSpace(body) == "" {
		return nil, nil
	}

	raw := splitChunks(body)
	if len(raw) > maxChunks {
		return nil, fmt.Errorf("diffpager: patch has %d files, over the %d-chunk limit", len(raw), maxChunks)
	}

	timeoutCtx, cancelTimeout := context.WithTimeout(context.Background(), renderTimeout)
	defer cancelTimeout()
	// A second, cancelable layer on top of the timeout lets the first failing
	// chunk stop every other in-flight (and not-yet-started) child process
	// immediately, instead of waiting out the rest of the 30s budget.
	ctx, cancelAll := context.WithCancel(timeoutCtx)
	defer cancelAll()

	args := substituteWidth(p.Args, width)

	workers := maxWorkers
	if n := runtime.GOMAXPROCS(0); n < workers {
		workers = n
	}
	if workers < 1 {
		workers = 1
	}

	out := make([]Chunk, len(raw))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var errOnce sync.Once
	var firstErr error

	for i, c := range raw {
		i, c := i, c
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			lines, err := p.renderChunk(ctx, dir, args, width, c.lines)
			if err != nil {
				errOnce.Do(func() {
					firstErr = err
					cancelAll()
				})
				return
			}
			out[i] = Chunk{Lines: lines, File: c.file}
		}()
	}
	wg.Wait()

	if firstErr != nil {
		// All-or-nothing: the caller's fallback is to re-render the whole body
		// with built-in styling, and a half-formatted patch would be worse than
		// either the formatter's output or the fallback alone.
		return nil, firstErr
	}
	return out, nil
}

// splitChunks breaks body into per-file spans on `diff --git` boundaries.
// Headers are matched against the ANSI-stripped line text, not the raw text:
// a user may put --color=always in their git diff args to feed a formatter
// that wants colored input, and a naive strings.HasPrefix would then find zero
// headers and collapse the whole patch into one chunk, which would also
// destroy the caller's file-jump index. Any text before the first header
// becomes its own File:false chunk — git bodies normally start with the
// header, but silently folding a preamble into the first file's chunk would
// lose content on the rare body that has one.
func splitChunks(body string) []rawChunk {
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return nil
	}
	lines := strings.Split(body, "\n")

	var chunks []rawChunk
	var cur []string
	curFile := false
	have := false

	flush := func() {
		if have {
			chunks = append(chunks, rawChunk{lines: cur, file: curFile})
		}
		cur = nil
		have = false
	}

	for _, line := range lines {
		if strings.HasPrefix(ansi.Strip(line), "diff --git") {
			flush()
			curFile = true
			have = true
		} else if !have {
			// First line(s) of the body, preceding any `diff --git` header.
			curFile = false
			have = true
		}
		cur = append(cur, line)
	}
	flush()
	return chunks
}

// substituteWidth replaces every occurrence of the literal "{width}" in args
// with width rendered as a decimal, falling back to 80 when width <= 0 (the
// diff viewer's width is only meaningful once a terminal has reported a
// size). Substitution happens within each argument, not just on whole-argument
// matches, so a formatter flag like "--width={width}" works.
func substituteWidth(args []string, width int) []string {
	if width <= 0 {
		width = 80
	}
	w := strconv.Itoa(width)
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = strings.ReplaceAll(a, "{width}", w)
	}
	return out
}

// renderChunk pipes one chunk's lines through the formatter and shapes its
// stdout back into lines. cmd.Dir is set to dir so a formatter that reads the
// repo's git config or .gitattributes (delta does) sees the repo it's running
// against, not ct's own working directory.
func (p Pager) renderChunk(ctx context.Context, dir string, args []string, width int, lines []string) ([]string, error) {
	cmd := exec.CommandContext(ctx, p.Command, args...)
	cmd.Dir = dir
	// The trailing newline is deliberate: splitChunks strips it, and a patch
	// whose final line arrives unterminated reads as a truncated one to a
	// formatter parsing line-wise.
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")

	// Several formatters (delta included) size their output from COLUMNS when
	// stdout isn't a terminal, which — piped into a Builder here — it never is.
	env := os.Environ()
	if width > 0 {
		env = append(env, "COLUMNS="+strconv.Itoa(width))
	}
	cmd.Env = env

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if len(args) > 0 {
			return nil, fmt.Errorf("%s %s: %s", p.Command, strings.Join(args, " "), msg)
		}
		return nil, fmt.Errorf("%s: %s", p.Command, msg)
	}

	out := strings.TrimSuffix(stdout.String(), "\n")
	if out == "" {
		return nil, nil
	}
	rawLines := strings.Split(out, "\n")
	result := make([]string, len(rawLines))
	for i, l := range rawLines {
		result[i] = strings.TrimRight(l, "\r")
	}
	return result, nil
}
