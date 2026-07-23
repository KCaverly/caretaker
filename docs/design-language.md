# ct design language

This document defines how `ct` should feel and behave. It is a product standard for future features, not a description of the code or a catalog of components.

`ct` is a calm, keyboard-first control deck for active development work. It should make a large amount of state feel legible without making the user feel monitored, rushed, or buried in chrome. The interface is compact because its users are capable, not because clarity is optional.

## Product character

**Calm under load.** The user may have many worktrees, agents, terminals, checks, and changes in flight. The interface should remain visually quiet. Stable context stays anchored; volatile state gets small, meaningful signals.

**Dense, then generous on demand.** Default views show the minimum useful facts. Selection, focus, or an explicit action reveals details. Do not make every row carry every fact.

**A deck, not a dashboard.** The interface is for steering work, not admiring metrics. Every persistent element should help the user orient, decide, or act.

**Terminal-native.** Work with cells, typography, glyphs, and keys. Do not imitate web controls. Preserve the host terminal's content and conventions whenever possible.

**Capable but forgiving.** Common actions should be fast enough for muscle memory. Dangerous actions should be slower, contextual, and easy to cancel.

**A little alive.** Motion, color, naming, and texture may add warmth, but never compete with work or obscure state.

## Core principles

### 1. Keep the user's place

The user should always know where they are, what is focused, and what will receive input.

- Keep global navigation and the active `repo / worktree` context in a stable location.
- Use one unmistakable focus treatment: a full-width selection bar for rows and an accent border for focused regions.
- Keep stable labels anchored while volatile facts appear beside them. Avoid layout shifts when counts or states change.
- When opening an overlay, retain the underlying conceptual context. Closing it returns the user to the same place and selection.
- Never silently change the active worktree, agent, pane, scope, or destructive target.

### 2. Reveal complexity progressively

Show enough to make the next decision, then let the user ask for more.

- A list row contains identity and the smallest useful state cluster.
- The selected row may expand one detail line containing only new information.
- Use a centered panel for a bounded choice or form.
- Use a full-body view for content that benefits from the viewport, such as diffs.
- Hide unavailable or inapplicable actions instead of showing dead controls. Show unavailable data only when its absence matters to the user's decision.
- Prefer omission over placeholder noise. Prefer an explicit empty state over unexplained blank space.

### 3. Make every action discoverable

Keyboard-first must not mean memory-first.

- Every action belongs in the command palette, using a verb-first, searchable label and its current key binding.
- Every view exposes the actions relevant to that view in a footer. Order hints by likely use; allow low-priority hints to fall away on narrow terminals.
- Help reflects configured keys, never hard-coded documentation that can drift.
- The first visit to a session may show a lightweight navigation hint. Retire coaching once the user begins working.
- Important status glyphs require a textual legend in Help and plain-language text in any decision panel.
- Mouse support is additive. Every mouse action must have a complete keyboard path.

### 4. Prefer direct manipulation, preserve expert speed

- `↑/↓` always moves within a vertical collection. Support `j/k` where focus is not inside an embedded program or text field.
- `Enter` performs the highlighted primary action: open, run, focus, or confirm.
- `Esc` backs out one layer without side effects. `q` may also close read-only, vim-like views.
- `Tab` moves between peer sections or form fields. `Shift+Tab` reverses.
- Arrow keys change segmented choices; show an inline `← → change` hint only on the focused choice.
- Single-letter mnemonics are acceptable in scoped views when shown in the footer. Global navigation uses configurable, collision-resistant chords.
- Preserve established mnemonics when replacing a prompt with a richer panel.
- Repeated attention-jump commands should cycle predictably rather than dead-end.

### 5. Protect flow across embedded tools

`ct` hosts editors, shells, and agents that already own most keys.

- Reserve as few global keys as possible and prefer configurable Alt chords or function keys.
- Forward all unreserved input faithfully to the active session.
- Never let a key or paste leak through a modal overlay into the session beneath it.
- Do not steal common editor or shell bindings for convenience.
- Pane focus, zoom, and splitting should use one spatial vocabulary across rendering, help, and command labels.

### 6. Treat destructive actions as decisions

- Never perform a destructive action from an ambiguous selection or a transient status line.
- Use a dedicated confirmation panel with a clear object identity and consequence.
- Put the safe option first and focus it by default.
- Use specific labels: `remove worktree, keep branch`, not `yes`; `quit ct`, not `confirm`.
- Show relevant evidence before the choice: branch divergence, uncommitted diffstat, busy work, or what will be interrupted.
- Offer review in the same flow when review can change the decision. Returning from review restores the confirmation context.
- Red is reserved for the destructive option and irreversible consequence, not the whole panel.
- Routine, reversible actions should remain frictionless. Confirmation is proportional to loss.

### 7. Communicate state redundantly

No important state may depend on color alone.

- Pair color with a glyph, word, position, or count.
- Use consistent semantics everywhere: `!` needs attention, `✓` is healthy or newly complete, `✷` is uncommitted, `●/○` is live/stopped, `…` is pending.
- Do not reuse a glyph for unrelated meanings within the same view.
- Status summaries should answer “what needs me?” before “what is happening?” Waiting input outranks unread completion; both outrank ambient activity.
- Use elapsed time for continuing work and plain words for terminal states.
- Do not animate semantic status. Update it promptly and let the content change be the signal.

### 8. Write like a capable teammate

Copy is concise, concrete, calm, and lowercase in working UI.

- Use verb-first commands: `open`, `view diff`, `split terminal right`, `restart agent`.
- Name the object when ambiguity is possible: `open caretaker/primary`.
- Use sentence fragments for status and hints; avoid terminal punctuation in compact UI.
- Use an ellipsis (`…`) only when an action opens a flow or work is genuinely continuing.
- Describe empty states as facts plus a next step when useful: `no workspaces yet — pick a repo above to create one`.
- Describe errors as `<action> error: <cause>`. Preserve actionable provider output, but do not expose internal jargon when a user-facing term exists.
- Avoid blame, celebration, and false urgency. Prefer `github unavailable — PR status omitted` to alarming language.
- Panel titles are short nouns or actions rendered uppercase by styling, not authored as shouting prose.
- Use `deck`, `worktree`, `agent`, `terminal`, and `pane` consistently. Do not introduce synonyms casually.

### 9. Earn delight

Delight comes from coherence, responsiveness, and small moments of character.

- The seedling, grove/deck metaphor, ambient plasma, provider color, and generated agent names give the product identity. Extend this vocabulary sparingly.
- Ambient visuals are optional, non-semantic, low-contrast, and automatically yield when space is scarce.
- Animate only ambient content or continuity that helps orientation. Respect a frozen/disabled setting and eventually a reduced-motion preference.
- A delightful response is immediate: selection moves on keypress, state is preserved, and background work never makes navigation feel blocked.
- Never use decoration in confirmation, error, diff, or attention-critical regions.

## Visual system

### Palette

The current palette is Gruvbox Dark, medium contrast. Preserve its warm, low-glare character and semantic roles.

| Token | Hex | Role |
|---|---:|---|
| Ink | `#1D2021` | Dark text on filled accent selections |
| Primary text | `#EBDBB2` | Names, selected content, primary facts |
| Secondary text | `#928374` | Explanations, metadata, key descriptions |
| Faint | `#665C54` | Rules, idle borders, disabled/inert chrome |
| Accent blue | `#83A598` | Focus, keys, interactive affordance, active terminal concepts |
| Purple | `#D3869B` | Agent identity and panel headings |
| Green | `#B8BB26` | Healthy, live, ahead, completed, additions |
| Yellow | `#FABD2F` | Deck identity, caution, dirty/recent/pending |
| Red | `#FB4934` | Error, destructive consequence, required attention, removals |

Rules:

- Assign color by meaning, not by feature preference.
- The accent border means focus. A faint border means structure without focus.
- Selection uses a neutral filled background with bright primary text. Use accent-filled chips only for small binary/segmented choices.
- Keep large surfaces unfilled so the terminal background remains part of the design.
- Do not add a new hue until existing semantic roles demonstrably cannot express the state.
- Check all important foreground/background pairs for readable contrast in common terminal renderers. When contrast is limited, strengthen with weight, glyph, or wording.

### Typography and symbols

- The terminal's monospace typeface is the type system. Hierarchy comes from weight, color, spacing, and position—not size.
- Bold is for titles, selected names, active icons, and decisive facts. Avoid bold paragraphs.
- Nerd Font icons may identify persistent top-level destinations. If icons are unavailable, fall back to stable text or ASCII; core navigation must remain usable.
- Use Unicode arrows and status marks when they reduce width, but always document them and pair critical ones with text.
- Align comparable numbers and right-side facts. Do not sacrifice names to preserve decorative spacing.

### Spacing and layout

- Maintain a two-row global bar: destinations/context, then a faint rule.
- Use two-cell leading indentation inside panels and lists.
- Separate conceptual groups with one blank row, not additional boxes.
- Center bounded panels and cap their readable width. Let content-heavy views use the viewport.
- Use rounded borders consistently for panels and deck regions.
- At narrow widths, remove ambient art, secondary hints, right-side facts, and optional metadata—in that order. Never truncate the focused identity or primary action before optional material.
- Below the viable size, show one clear resize instruction rather than a broken layout.

## Interaction patterns

### Global bar

The bar is an orientation rail, not a toolbar. It contains top-level destinations on the left and attention plus current context on the right. Icons become vivid only when active; unavailable destinations remain faint. Click targets mirror keyboard navigation but are never the only route.

### Deck

The deck is the home surface. `NEW` and `ACTIVE` are peers, with exactly one focused region. Filtering belongs to `NEW`; worktree management belongs to `ACTIVE`. Group rows by repository. Keep branch state right-aligned and reveal commit/diff/stack context only for the selected worktree.

### Command palette

The palette is the universal escape hatch and learning surface. It starts empty, fuzzy-filters immediately, retains a visible selected row, displays live bindings, and runs the selected command with `Enter`. Commands must be available only when meaningful, or safely no-op with an explicit explanation.

### Overlays

Use a centered overlay for one bounded job: choose an agent, inspect usage, create something, confirm something, or view help. One overlay at a time. Every overlay has a title, clear focus, a footer, and an `Esc` path. Read-only overlays swallow unrelated keys; they do not accidentally act on the hidden surface.

### Forms

Ask only for information the system cannot infer. Start focus on the field most likely to need typing. Hide rows where there is no choice. Make defaults visible. Multi-line prompts use `Enter` for a newline and a distinct, displayed chord to submit.

### Loading, empty, success, and error

- Loading: present-progress wording such as `scanning…`, `loading diff…`, or `working…`; no fake percentages.
- Empty: explain what is absent and, when not obvious, the next action.
- Success: brief transient feedback for actions whose result is not already visually obvious.
- Error: persistent enough to read, colored red, scoped to the failed action, and recoverable without losing the user's place.
- Stale/unavailable: omit data that would mislead; explicitly note omissions when the user requested that data.

### Notifications and attention

Notifications are work routing, not a feed. Aggregate them in the bar, identify urgency by glyph and count, and make the summary actionable. Visiting the relevant agent acknowledges unread completion; merely passing through unrelated views does not. Bells and visual signals should be configurable and rate-limited.

## Accessibility standard

Every feature must meet all of the following:

- Fully operable without a mouse.
- Visible focus at every navigable step.
- No critical meaning conveyed by color alone.
- No required dependency on a Nerd Font glyph without a fallback or textual explanation.
- Key bindings shown from live configuration.
- Logical navigation order that matches visual order.
- Modal focus containment and no input leakage to embedded sessions.
- Stable behavior when the terminal is resized, narrow, short, or using a different monospace font.
- Motion is optional and non-essential; frozen/disabled animation leaves a complete experience.
- Plain-language warnings name the affected object and consequence.
- Copy and symbols remain understandable with color disabled.

## Audit of the current language

### Strong foundations to preserve

- The persistent bar provides excellent orientation while leaving nearly the entire screen to the user's tools.
- The deck uses progressive disclosure well: compact worktree rows expand only the selected row.
- Focused borders, full-row selection, right-aligned facts, and consistent footer hints form a coherent spatial grammar.
- The command palette turns a broad keyboard surface into a discoverable one and teaches configured shortcuts passively.
- Confirmation panels are unusually strong: safe defaults, concrete consequences, direct mnemonics, and a review-before-removal loop.
- Attention signals prioritize required input and completed work without turning the interface into a notification center.
- Forms hide irrelevant choices and clearly separate multi-line entry from submission.
- The ambient plasma gives the otherwise austere deck warmth and identity while yielding on narrow screens.

### Standards to tighten as the product grows

- **Glyph resilience:** the status bar currently assumes a Nerd Font. Provide ASCII/text fallbacks so identity never becomes missing boxes.
- **Color validation:** document and test contrast for primary, dim, faint, selected, and danger states across common terminal backgrounds. Faint text must remain readable, especially in footers.
- **Legend consistency:** the same marks can represent related but not identical concepts (`✓` healthy stack vs unread completion). Context currently disambiguates them; future features must not stretch this vocabulary further without labels.
- **Key consistency:** some lists accept `j/k`, others only arrows/Control navigation. Apply the collection rules above unless an embedded input owns those keys.
- **Close vocabulary:** use `back` when returning to an originating view, `close` for dismissing an overlay, and `cancel` when abandoning an uncommitted decision.
- **Responsive priorities:** codify which facts disappear first so new status segments do not crowd the anchored `repo / worktree` identity.
- **Motion accessibility:** plasma can be frozen or disabled, but reduced-motion behavior should become an explicit product preference.
- **First-run learning:** Help and the palette are strong once discovered. Ensure first-run guidance teaches the deck/session boundary, command palette, and global Help without becoming persistent onboarding chrome.

## Feature review checklist

Before shipping any feature, answer yes to each applicable question:

1. Can the user describe where they are and what is focused?
2. Is the primary action visible in the footer or command palette?
3. Can the entire flow be completed with the keyboard alone?
4. Are keys collision-safe inside hosted editors, agents, and shells?
5. Does `Esc` back out safely, and can any key leak through an overlay?
6. Is important state understandable without color or a Nerd Font?
7. Are loading, empty, unavailable, success, and error states defined?
8. Does the feature preserve selection and context when opened and closed?
9. Is every destructive consequence named, evidenced, safely defaulted, and reviewable where practical?
10. Does the copy use existing nouns and verb-first actions?
11. Does the narrow layout preserve identity, focus, and the primary action?
12. Is any animation optional, non-semantic, and calm?
13. Does the feature reduce or clarify work rather than merely expose more state?
14. Has it reused the existing palette and interaction grammar before adding a new one?

When a design cannot satisfy a rule, document the exception and the user benefit that justifies it. Exceptions should be rare, local, and reversible.
