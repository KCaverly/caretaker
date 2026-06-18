# ct

A terminal "deck" for managing git worktrees across your repos. Each worktree gets a
**workspace** — an nvim window, a claude session, and a terminal — hosted *inside* ct under a
pinned status bar, with one session full-screen at a time.

Built with [Bubble Tea](https://charm.land/bubbletea) v2 and [charmbracelet/x/vt](https://github.com/charmbracelet/x).

## Requirements

- Go 1.25+
- `git`, and your editor/agent commands (`nvim`, `claude`) on `PATH`

## Configuration

`ct` reads `~/.config/ct/config.toml` (honoring `XDG_CONFIG_HOME`). Only `root` is required:

```toml
# Parent directory containing your repos (each immediate child with a .git).
root = "~/code"

# Optional (defaults shown):
editor = "nvim"
agent  = "claude"
shell  = "$SHELL"

# Where new worktrees and their branches are created ({name} is the worktree name).
worktree_path = ".worktrees/{name}"
branch_name   = "{name}"

# Reserved navigation keys (not forwarded to embedded sessions).
[keys]
cycle  = "ctrl+o"   # move one session view to the right
picker = "ctrl+g"   # return to the CT picker
```

## Run

```sh
go run ./cmd/ct
# or
go build -o ct ./cmd/ct && ./ct
```

## How it works

A pinned status bar sits at the top at all times:

The bar is a row of spaced **Nerd Font** glyphs (a Nerd Font is required). The caretaker shows a
yellow smiley while you tend the deck and a red skull once you drop into a session; the nvim
(code), claude (robot), and term (terminal) icons glow in their own colour when active and dim
otherwise (faint until a workspace exists). The active repo / worktree shows on the right.

- **Picker** (smiley): the deck — a `NEW` repo fuzzy-finder and an `ACTIVE` list of your
  worktrees grouped by repo (`●` running · `○` stopped · `✷` uncommitted changes). Within each
  repo, worktrees are ordered by when you last opened them in ct (most recent first), falling
  back to git commit time for ones you haven't opened yet. The three most-recently-opened
  worktrees overall get a `1`/`2`/`3` rank in the left column. Last-opened times are persisted to
  `$XDG_STATE_HOME/ct/state.json` (default `~/.local/state/ct/state.json`).
- Pressing **enter** on a worktree **activates** it: ct starts nvim + claude + a terminal in
  that worktree and drops you into the nvim view. The session segments light up and the active
  repo / worktree shows on the right.
- **`ctrl+o`** cycles the session views (nvim → claude → terminal → nvim); **`ctrl+g`** returns
  to the picker — and from the picker, **`ctrl+g`** jumps straight to your most recently opened
  worktree, so it toggles you between the deck and your latest work. You can also **click** any
  of the four bar icons to jump straight to that view (the session icons are inert until a
  workspace is active). Sessions keep running — switching never relaunches them, and they
  persist for ct's lifetime.

Picker keys: `tab` switch section · `enter` open · `d` stop · `x` remove · `r` refresh · `ctrl+c` quit.

## Layout

- `cmd/ct` — the `ct` entrypoint.
- `internal/config` — config loading + defaults.
- `internal/repo` — repo discovery and git worktree operations.
- `internal/session` — hosts programs on ptys with a terminal emulator (`x/vt`) per session.
- `internal/tui` — the Bubble Tea model: status bar, picker, key routing, session rendering.
