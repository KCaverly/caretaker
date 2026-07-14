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
# Defaults use alt (option-as-meta) chords so they don't collide with the
# programs running inside the panes (LazyVim, zsh, Claude Code); every key is
# overridable, and an empty string ("") disables one.
[keys]
cycle      = "alt+]"   # next session view (wraps)
cycle_back = "alt+["   # previous session view (wraps)
goto_editor = "alt+1"  # jump to the editor view
goto_agent  = "alt+2"  # jump to the agent view
goto_term   = "alt+3"  # jump to the terminal view
picker      = "ctrl+g" # return to the CT picker
help        = "f1"     # toggle the key/legend overlay (also "?" in the deck)
command_palette = "alt+p" # fuzzy-searchable list of every action + its key

# Terminal-screen-only pane keys.
term_split_v    = "alt+v"  # new pane to the right
term_split_h    = "alt+s"  # new pane below
term_zoom       = "alt+z"  # zoom / restore the focused pane
term_close      = "alt+x"  # close the focused pane
term_focus_left = "alt+h"  # directional pane focus (h/j/k/l)
term_focus_down = "alt+j"
term_focus_up   = "alt+k"
term_focus_right = "alt+l"

# Ambient plasma panel on the right of the deck (defaults shown). It only
# animates while the deck is on screen, and hides itself on terminals too
# narrow to split.
[plasma]
pattern = "classic" # classic | waves | interference | lava | ripple
palette = "aurora"  # aurora (blue/purple) | ember (yellow/red) | mono (grayscale)
charset = "dots"    # dots (braille) | shade | blocks
speed   = 0.3       # animation rate; 0 freezes the pattern
width   = 40        # percent of the terminal width; 0 disables the panel
```

## Run

```sh
go run ./cmd/ct
# or
go build -o ct ./cmd/ct && ./ct
```

## How it works

A pinned status bar sits at the top at all times:

The bar is a row of spaced **Nerd Font** glyphs (a Nerd Font is required). The caretaker is a
seedling, lit yellow while you tend the deck and dim once you drop into a session; the nvim (code),
claude (robot), and term (terminal) icons glow in their own colour when active and dim otherwise
(faint until a workspace exists). When an agent is waiting on your input, the `! N` badge on the
right signals it. The active repo / worktree shows on the right.

- **Picker** (seedling): the deck — a `NEW` repo fuzzy-finder and an `ACTIVE` list of your
  worktrees grouped by repo (`●` running · `○` stopped · `✷` uncommitted changes). Within each
  repo, worktrees are ordered by when you last opened them in ct (most recent first), falling
  back to git commit time for ones you haven't opened yet. The three most-recently-opened
  worktrees overall get a `1`/`2`/`3` rank in the left column. Each row also shows how far its
  branch is ahead/behind the repo's main branch (`↑N ↓M`, right-aligned), and the selected row
  expands a `└` detail line with the divergence, uncommitted diffstat, last commit subject, and
  age. Last-opened times are persisted to
  `$XDG_STATE_HOME/ct/state.json` (default `~/.local/state/ct/state.json`).
- Pressing **enter** on a worktree **activates** it: ct starts nvim + claude + a terminal in
  that worktree and drops you into the nvim view. The session segments light up and the active
  repo / worktree shows on the right. You can also **click** a deck row to select it and click it
  again to open it (or, in `NEW`, to start naming a worktree).
- **`alt+]`** / **`alt+[`** cycle the session views forward / backward
  (nvim → claude → terminal → nvim, wrapping), and **`alt+1`/`alt+2`/`alt+3`** jump straight to
  the editor, agent, or terminal view. **`ctrl+g`** returns to the picker — and from the picker,
  **`ctrl+g`** jumps straight to your most recently opened worktree, so it toggles you between the
  deck and your latest work. You can also **click** any of the four bar icons to jump straight to
  that view (the session icons are inert until a workspace is active). Sessions keep running —
  switching never relaunches them, and they persist for ct's lifetime.

Picker keys: `tab` switch section · `enter` open · `d` stop · `v` view diff · `x` remove · `r` refresh · `?` help · `ctrl+c` quit.

`v` opens a read-only diff of everything the branch carries beyond main (committed + uncommitted;
`u` narrows it to just the uncommitted changes), also offered from the remove prompt so you can
review a worktree before deleting it.

Press **`?`** (in the deck) or **`f1`** (anywhere, including inside a session) for a key + legend
overlay; any key closes it.

Press **`alt+p`** (anywhere) for the **command palette**: a fuzzy-searchable list of every ct action
with its live key shown alongside, so you can run any action — and passively learn its chord —
without memorizing the reserved keys; `enter` runs the selected row, `esc` closes.

## Layout

- `cmd/ct` — the `ct` entrypoint.
- `internal/config` — config loading + defaults.
- `internal/repo` — repo discovery and git worktree operations.
- `internal/session` — hosts programs on ptys with a terminal emulator (`x/vt`) per session.
- `internal/tui` — the Bubble Tea model: status bar, picker, key routing, session rendering.
