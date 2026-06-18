# ct

A terminal "deck" for managing git worktrees across your repos. Each worktree gets a
**workspace** — an nvim window, one or more claude sessions, and one or more terminals —
hosted by [zellij](https://zellij.dev), with one session full-screen at a time.

Built with [Bubble Tea](https://charm.land/bubbletea) v2.

## Requirements

- Go 1.25+
- `git`, `zellij`, and your editor/agent commands (`nvim`, `claude`) on `PATH`

## Configuration

`ct` reads `~/.config/ct/config.toml` (honoring `XDG_CONFIG_HOME`). Only `root` is required:

```toml
# Parent directory containing your repos (each immediate child with a .git).
root = "~/code"

# Optional (defaults shown):
editor  = "nvim"
agent   = "claude"
shell   = "$SHELL"
backend = "zellij"

# Where new worktrees and their branches are created ({name} is the worktree name).
worktree_path = ".worktrees/{name}"
branch_name   = "{name}"
```

## Run

```sh
go run ./cmd/ct
# or
go build -o ct ./cmd/ct && ./ct
```

### The deck

```
ct

caretaker
  ▸ ● ✷ feat-login    feat-login
    ○   (main)        main

other-repo
    ○   (main)        main
```

`●` = workspace running · `○` = stopped · `✷` = uncommitted changes.

Keys: `enter` open · `n` new worktree · `a` +claude · `t` +terminal · `d` archive ·
`x` remove · `r` refresh · `q` quit.

Opening a worktree creates (if needed) a zellij session with nvim + claude + term tabs and
attaches you full-screen; detach from zellij to return to the deck.

## Layout

- `cmd/ct` — the `ct` entrypoint.
- `internal/config` — config loading + defaults.
- `internal/repo` — repo discovery and git worktree operations.
- `internal/workspace` — the backend-agnostic workspace model.
- `internal/backend` — the `Backend` interface; `internal/backend/zellij` implements it.
- `internal/tui` — the Bubble Tea deck and its controller.

A native PTY backend (no zellij dependency, with live per-session status) can be added behind
the same `Backend` interface later.
