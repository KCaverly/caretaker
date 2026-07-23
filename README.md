# ct

A terminal "deck" for managing git worktrees across your repos. Each worktree gets a
**workspace** — an nvim window, one or more Claude Code or Codex agents, and a terminal — hosted
*inside* ct under a pinned status bar, with one session full-screen at a time.

Built with [Bubble Tea](https://charm.land/bubbletea) v2 and [charmbracelet/x/vt](https://github.com/charmbracelet/x).

![The ct deck showing repositories, active worktrees, branch divergence, and the ambient plasma panel](docs/screenshots/worktree-deck.png)

## At a glance

### One agent board

Run Claude and Codex side by side, see what needs attention, and jump between agents across every
active worktree.

![The agent board showing Claude and Codex agents across multiple worktrees](docs/screenshots/agent-board.png)

### Persistent terminal panes

Split, focus, and zoom terminals while tests, servers, and monitoring tools keep running.

![A workspace with three terminal panes running tests, a development server, and a process monitor](docs/screenshots/split-terminal-workspace.png)

### Everything is discoverable

Search every action and see its current configurable shortcut without leaving the workspace.

![The searchable ct command palette with live keyboard shortcuts](docs/screenshots/command-palette.png)

## Requirements

- Go 1.25+
- `git` and your configured editor (`nvim` by default) on `PATH`
- The CLI for every enabled agent provider on `PATH` and already authenticated:
  [Claude Code](https://docs.anthropic.com/en/docs/claude-code) and/or
  [Codex](https://github.com/openai/codex)
- Codex integration uses the experimental remote/App Server interface and is tested with
  `codex-cli 0.144.4`; use that version or newer

## Credentials and privacy

`ct` does not implement its own agent login or store copies of provider credentials. To display
Claude usage limits, it reads Claude Code's existing OAuth credential from the macOS Keychain (or
`~/.claude/.credentials.json` as a fallback) and sends it only to Anthropic's
`https://api.anthropic.com/api/oauth/usage` endpoint. Codex usage and lifecycle information comes
from a local `codex app-server` process started from your configured Codex command. `ct` has no
telemetry and does not send repository contents, prompts, or session output to a caretaker service.

## Configuration

`ct` reads `~/.caretaker/config.toml`, or the path in `CT_CONFIG`. Only `root` is required:

```toml
# Parent directory containing your repos (each immediate child with a .git).
root = "~/code"

# Optional program defaults:
editor = "nvim"
shell  = "/bin/zsh" # defaults to the current $SHELL when omitted

# Where new worktrees and their branches are created ({name} is the worktree name).
worktree_path = ".worktrees/{name}"
branch_name   = "{name}"

# Both providers are enabled by default, with Claude initially selected, so
# this whole section may be omitted. `default` must also appear in `enabled`.
[agents]
default = "claude"
enabled = ["claude", "codex"]

[agents.claude]
command = "claude"
args = []

[agents.codex]
command = "codex"
args = []

# Reserved navigation keys (not forwarded to embedded sessions).
# Defaults use alt (option-as-meta) chords so they don't collide with the
# programs running inside the panes (LazyVim, zsh, Claude Code, Codex); every key is
# overridable, and an empty string ("") disables one.
[keys]
cycle      = "alt+]"   # next session view (wraps)
cycle_back = "alt+["   # previous session view (wraps)
goto_editor = "alt+1"  # jump to the editor view
goto_agent  = "alt+2"  # jump to the agent view
goto_term   = "alt+3"  # jump to the terminal view
picker      = "ctrl+g" # return to the CT picker
palette     = "alt+a"  # agent board: focus, create, restart, or remove agents
next_agent  = "f4"     # next agent in the active worktree
prev_agent  = "f3"     # previous agent in the active worktree
global_config = "alt+g" # open the home-directory workspace
attention   = "alt+n"  # jump to the agent needing attention (cycles on repeat)
back        = "alt+b"  # return to the previous work location (toggles on repeat)
usage       = "alt+u"  # plan usage for enabled agent providers
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

# Stack merges require a confirmation by default. This setting bypasses ct's
# confirmation; it is not GitHub's "auto-merge when checks pass" feature.
[stack]
auto_merge = false

# Persistent navigation and pane symbols. `nerd` preserves the default icon
# set; `text` uses labels; `ascii` uses stable single-cell characters.
[display]
icons = "nerd" # nerd | text | ascii
```

The old top-level `agent = "claude"` setting is still accepted as a Claude command override.
New configurations should use `[agents.claude]` instead. `command` is the executable and `args`
is a list of base arguments inserted immediately after it, which also supports wrappers:

```toml
[agents]
default = "codex"
enabled = ["codex"]

[agents.codex]
command = "mise"
args = ["exec", "--", "codex"]
```

## Run

```sh
go run ./cmd/ct
# or
go build -o ct ./cmd/ct && ./ct
```

## Install and update

Release binaries currently support Apple Silicon and Intel Macs. They do not
require Go or a caretaker source checkout.

With Homebrew:

```sh
brew install kcaverly/tap/ct
brew upgrade ct
```

Without Homebrew, use the checksum-verifying installer:

```sh
curl -fsSL https://raw.githubusercontent.com/KCaverly/caretaker/main/scripts/install.sh | sh
```

The installer writes to `~/.local/bin` by default. Set `CT_INSTALL_DIR` to use a
different directory, or download it first if your environment does not permit
piping scripts into a shell. A specific release can be installed with
`--version v0.1.0`. Confirm the installed build with `ct version`.

### Maintainer release process

Releases are built by GitHub Actions from semantic version tags and begin as
drafts so their archives and attestations can be reviewed before publication.

1. Create the public `KCaverly/homebrew-tap` repository.
2. Add a fine-grained `HOMEBREW_TAP_TOKEN` repository secret with Contents
   read/write access to that tap. Until then, releases still work but tap
   publication is skipped.
3. Ensure the release commit is on `main`, then push a tag such as `v0.1.0`.
4. Review the draft release, its two macOS archives, checksums, generated notes,
   and attestations, then publish it.
5. Enable immutable releases in repository settings after the first successful
   end-to-end release.

Release assets can be verified with GitHub CLI:

```sh
gh attestation verify ct_Darwin_arm64.tar.gz --repo KCaverly/caretaker
```

## How it works

A pinned status bar sits at the top at all times:

The bar is a row of spaced **Nerd Font** glyphs (a Nerd Font is required). The caretaker is a
seedling, lit yellow while you tend the deck and dim once you drop into a session; the nvim (code),
agent (robot), and term (terminal) icons glow in their own colour when active and dim otherwise
(faint until a workspace exists). Lifecycle status comes from `claude agents --json` for Claude
and structured App Server events for Codex. When an agent is waiting on your input, the `! N` badge
on the right signals it, and completed agents are promoted in the agent board. The active repo /
worktree shows on the right.

- **Picker** (seedling): the deck — a `NEW` repo fuzzy-finder and an `ACTIVE` list of your
  worktrees grouped by repo (`●` running · `○` stopped · `✷` uncommitted changes). Within each
  repo, worktrees are ordered by when you last opened them in ct (most recent first), falling
  back to git commit time for ones you haven't opened yet. The three most-recently-opened
  worktrees overall get a `1`/`2`/`3` rank in the left column. Each row also shows how far its
  branch is ahead/behind the repo's main branch (`↑N ↓M`, right-aligned), and the selected row
  expands a `└` detail line with new context only: uncommitted diffstat, last commit subject, age,
  and (when available) pull-request state. Last-opened times are persisted to
  `$XDG_STATE_HOME/ct/state.json` (default `~/.local/state/ct/state.json`).
- Pressing **enter** on a worktree **activates** it: ct starts nvim + an agent from
  `agents.default` + a terminal in that worktree and drops you into the nvim view. The session
  segments light up and the active repo / worktree shows on the right. You can also **click** a
  deck row to select it and click it again to open it (or, in `NEW`, to start naming a worktree).
- **`alt+]`** / **`alt+[`** cycle the session views forward / backward
  (nvim → agent → terminal → nvim, wrapping), and **`alt+1`/`alt+2`/`alt+3`** jump straight to
  the editor, agent, or terminal view. **`ctrl+g`** returns to the picker — and from the picker,
  **`ctrl+g`** jumps straight to your most recently opened worktree, so it toggles you between the
  deck and your latest work. You can also **click** any of the four bar icons to jump straight to
  that view (the session icons are inert until a workspace is active). Sessions keep running —
  switching never relaunches them, and they persist for ct's lifetime.

### Agent providers and lifecycle

Press **`alt+a`** to open the agent board for every active worktree. From there, `n` creates an
agent, `enter` focuses one, `r` restarts it in place, and `d` removes it.

Press **`alt+n`** (from anywhere) to jump straight into the session of the agent that most needs
you — agents waiting on input first, then unread completions — without opening the board; pressing
it again cycles to the next agent needing attention. Clicking the `! N` badge in the status bar does
the same thing. **`alt+b`** returns to the work location from before the last attention jump or
cross-worktree activation, restoring its screen and focused agent or terminal pane; press it again
to toggle back. When more than one provider
is enabled, the new-agent form adds a Claude/Codex selector; when only one is enabled, that row is
hidden. Each time the form opens it starts on `agents.default`. Board and status-bar labels include
the provider so mixed pools remain easy to distinguish.

Caretaker passes the form prompt as the CLI's initial prompt; agents launch interactively.
The prompt editor supports multiple lines; press `ctrl+enter` to launch the agent (plain `enter`
adds a new line). Codex starts a fresh conversation normally and uses `codex resume <thread-id>`
when a known thread ID is restored.

Each Codex pane also owns a private companion `codex app-server` on a local Unix socket. The stock
Codex TUI connects to it with `--remote` and remains responsible for approvals and user input;
caretaker's passive connection observes thread, turn, waiting, completion, failure, and disconnect
events. This remote/App Server interface is experimental, which is why the Codex CLI version in the
requirements matters.

Agent state records the provider, conversation ID, display label, pool order, and focused agent in
`$XDG_STATE_HOME/ct/state.json` (default `~/.local/state/ct/state.json`). Older state without a
provider is migrated to Claude. On the next activation, ct resumes conversations with stored IDs.
Restart is transactional: ct starts the replacement before swapping it into the same pool position,
so a bad command leaves the current process running.

Fresh Claude sessions receive a caretaker-managed ID immediately. Fresh Codex thread IDs are
assigned by Codex and captured from the pane's App Server as soon as the thread starts. Both are then
persisted and resumed after restarting ct. Restarting an agent from the board preserves its provider,
conversation, label, pool position, and focus. Codex lifecycle events feed the same busy, waiting,
completion, attention, and bell system as Claude; failure of Claude's status poll does not erase
Codex status. The `alt+u` usage panel includes estimates for every enabled provider. The top-bar
gauge follows the focused agent, so a Claude session never shows Codex usage and vice versa.

Picker keys: `tab` switch section · `enter` open · `d` stop · `v` view diff · `x` remove · `r` refresh · `?` help · `ctrl+c` quit.

`x` opens a centered **remove** panel rather than a one-line prompt: it shows the worktree's
divergence and uncommitted diffstat (with a red warning when the tree is dirty) above a vertical
list of options — `remove worktree, keep branch` (the safe default the cursor starts on),
`remove worktree and delete branch` (destructive, red), `view diff first`, and `cancel`. Arrow
keys (or `j`/`k`) move and `enter` fires the highlighted option, while the mnemonics still work
directly, so the old `x` `b` (keep branch) and `x` `y` (delete branch) muscle memory is
unchanged. The quit and stop guards use the same panel, defaulting to `cancel`.

`v` opens a read-only diff of everything the branch carries beyond main (committed + uncommitted;
`u` narrows it to just the uncommitted changes), also offered from the remove panel (`view diff
first`) so you can review a worktree before deleting it — and `x` from the diff loops back to the
panel.

Press **`?`** (in the deck) or **`f1`** (anywhere, including inside a session) for a key + legend
overlay; any key closes it.

Press **`alt+p`** (anywhere) for the **command palette**: a fuzzy-searchable list of every ct action
with its live key shown alongside, so you can run any action — and passively learn its chord —
without memorizing the reserved keys; `enter` runs the selected row, `esc` closes.

## Stacked PRs

ct treats each commit on a worktree branch as its own GitHub PR, chained bottom-to-top and
identified by a `ct-stack-id` commit trailer. The `ct stack` CLI drives the workflow from a
worktree:

- `ct stack status` — read-only reconciliation of local commits, last-fetched remote branches, and
  PR state into a per-commit status plus a stack-level `next_action` hint (`--json` for the machine
  form). It never mutates anything.
- `ct stack submit` — the additive half: assigns ids to new commits, pushes branches, and creates or
  updates PRs (retarget/retitle/nav-table) to match the local stack. `--dry-run` prints the plan.
- `ct stack restack` — repairs a stack after its bottom PRs squash-land: drops the landed commits
  from the local branch, deletes their remote branches, and re-submits the survivors. `--dry-run`
  prints the plan; because it rewrites branches, run the plan first.
- `ct stack merge` — squash-merges the bottom PR with its original commit message. It leaves branch
  cleanup to GitHub's repository setting so dependent stacked PRs are retargeted before the merged
  head branch is deleted. It re-fetches immediately before merging and refuses unless GitHub
  reports `MERGEABLE` and the PR targets the repository's main branch. After merging, it waits
  through GitHub's brief retargeting and mergeability-calculation window before publishing the next
  stack status, so transient `restack`/`wait` states do not flash in the UI.

### Stacked PRs in the TUI

The deck surfaces stack state passively, without ever running a subprocess on the render path (a
cache is refreshed after each deck load and on `r`, and cleanly shows nothing until data lands or
when GitHub is unavailable):

- **Deck glyph** — after a worktree row's `↑N ↓M` cluster: a red `↻` when the stack needs a restack
  (commits landed below), a green `✓` when every commit is open with checks passing, a yellow `…`
  when any PR's checks are pending, and a red `!` for conflicts or escalations (closed PR, duplicate
  id, broken base chain). Nothing shows for an unsubmitted stack or when GitHub is unavailable — the row stays
  exactly as it was.
- **Detail line** — the selected worktree's expanded line gains a stack segment: a single-commit
  stack reads `PR #42 open · checks passing`; larger stacks skip the redundant size and state the useful
  outcome directly, such as `1 merged · restack needed`, `resolve conflicts`, or `waiting on checks`.
- **Conflict recovery** — when a PR conflicts after an earlier stack commit lands, the stack screen
  keeps the conflict visible and offers `R` to preview a restack that drops the landed prefix and
  rebases the remaining commits onto current `origin/main`.
- **Merge action** — a bottom PR that GitHub reports as mergeable and that targets main exposes
  `M merge` in the stack screen and a matching command-palette action. Non-main, conflicting, and
  unknown-mergeability PRs never expose the action.
- **Command palette** — per active worktree: `stack status: <repo>/<wt>` (always), `restack:
  <repo>/<wt>` (only when a restack is needed, hinted with the landed count), and `submit stack:
  <repo>/<wt>` (only with submit-able work).

All three verbs open a read-only, scrollable **stack overlay** titled `STACK <repo> / <wt>` showing
the same output as `ct stack status`. `submit` runs directly (it is additive) and reports the fresh
status back into the overlay. `restack` shows its **dry-run plan first** with an `enter run · esc
cancel` footer — `enter` executes it for real, `esc` cancels without touching your branches. Inside
the overlay, `j`/`k` scroll, `r` re-fetches, and `esc`/`q` close.

## Layout

- `cmd/ct` — the `ct` entrypoint.
- `internal/config` — config loading + defaults.
- `internal/repo` — repo discovery and git worktree operations.
- `internal/session` — hosts programs on ptys with a terminal emulator (`x/vt`) per session.
- `internal/tui` — the Bubble Tea model: status bar, picker, key routing, session rendering.
