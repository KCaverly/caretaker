package session

import (
	"reflect"
	"testing"
)

// leaf builds a leaf PaneNode.
func leaf(idx int) *PaneNode { return &PaneNode{Dir: SplitNone, Idx: idx} }

// split builds an internal PaneNode.
func split(dir SplitDir, ratio float64, a, b *PaneNode) *PaneNode {
	return &PaneNode{Dir: dir, Ratio: ratio, A: a, B: b}
}

// --- ComputePaneBounds ---

func TestComputePaneBoundsSingleLeaf(t *testing.T) {
	bounds := ComputePaneBounds(leaf(0), 0, 0, 80, 24)
	if len(bounds) != 1 {
		t.Fatalf("expected 1 bound, got %d", len(bounds))
	}
	want := PaneBounds{X: 0, Y: 0, W: 80, H: 24, Idx: 0}
	if bounds[0] != want {
		t.Errorf("got %+v, want %+v", bounds[0], want)
	}
}

func TestComputePaneBoundsVerticalSplit(t *testing.T) {
	root := split(SplitV, 0.5, leaf(0), leaf(1))
	bounds := ComputePaneBounds(root, 0, 0, 81, 24) // 81 = 40 + 1 + 40
	if len(bounds) != 2 {
		t.Fatalf("expected 2 bounds, got %d", len(bounds))
	}
	// A is left: X=0, W=40
	if bounds[0].X != 0 || bounds[0].W != 40 || bounds[0].H != 24 {
		t.Errorf("left pane: %+v", bounds[0])
	}
	// B is right: X=41, W=40
	if bounds[1].X != 41 || bounds[1].W != 40 || bounds[1].H != 24 {
		t.Errorf("right pane: %+v", bounds[1])
	}
	if bounds[0].Idx != 0 || bounds[1].Idx != 1 {
		t.Errorf("wrong indices: %d %d", bounds[0].Idx, bounds[1].Idx)
	}
}

func TestComputePaneBoundsHorizontalSplit(t *testing.T) {
	root := split(SplitH, 0.5, leaf(0), leaf(1))
	bounds := ComputePaneBounds(root, 0, 0, 80, 25) // 25 = 12 + 1 + 12
	if len(bounds) != 2 {
		t.Fatalf("expected 2 bounds, got %d", len(bounds))
	}
	if bounds[0].Y != 0 || bounds[0].H != 12 || bounds[0].W != 80 {
		t.Errorf("top pane: %+v", bounds[0])
	}
	if bounds[1].Y != 13 || bounds[1].H != 12 || bounds[1].W != 80 {
		t.Errorf("bottom pane: %+v", bounds[1])
	}
}

func TestComputePaneBoundsThreePanes(t *testing.T) {
	// Layout: left=0, right={top=1, bottom=2}
	root := split(SplitV, 0.5, leaf(0), split(SplitH, 0.5, leaf(1), leaf(2)))
	bounds := ComputePaneBounds(root, 0, 0, 81, 25)
	if len(bounds) != 3 {
		t.Fatalf("expected 3 bounds, got %d", len(bounds))
	}
	// Left pane spans full height
	if bounds[0].X != 0 || bounds[0].H != 25 {
		t.Errorf("left pane %+v", bounds[0])
	}
	// Right panes start at X=41
	if bounds[1].X != 41 || bounds[2].X != 41 {
		t.Errorf("right panes wrong X: %d %d", bounds[1].X, bounds[2].X)
	}
	// Top and bottom right panes have different Y
	if bounds[1].Y == bounds[2].Y {
		t.Error("top and bottom right panes should have different Y")
	}
}

func TestComputePaneBoundsTooNarrowToSplit(t *testing.T) {
	root := split(SplitV, 0.5, leaf(0), leaf(1))
	bounds := ComputePaneBounds(root, 0, 0, 2, 24) // too narrow
	if len(bounds) != 1 {
		t.Fatalf("expected 1 bound (fallback to A), got %d", len(bounds))
	}
	if bounds[0].Idx != 0 {
		t.Errorf("expected A (idx 0) as fallback, got idx %d", bounds[0].Idx)
	}
}

// --- SplitPaneNode ---

func TestSplitPaneNodeFromLeaf(t *testing.T) {
	root := leaf(0)
	newRoot := SplitPaneNode(root, 0, 1, SplitV)
	if newRoot.Dir != SplitV {
		t.Fatalf("expected SplitV root, got %v", newRoot.Dir)
	}
	if newRoot.A == nil || newRoot.B == nil {
		t.Fatal("expected two children")
	}
	if newRoot.A.Idx != 0 || newRoot.B.Idx != 1 {
		t.Errorf("wrong indices: A=%d B=%d", newRoot.A.Idx, newRoot.B.Idx)
	}
}

func TestSplitPaneNodeNestedSplit(t *testing.T) {
	// Split leaf 0 vertically, then split leaf 1 horizontally.
	root := SplitPaneNode(leaf(0), 0, 1, SplitV)
	root = SplitPaneNode(root, 1, 2, SplitH)
	leaves := PaneLeaves(root)
	want := []int{0, 1, 2}
	if !reflect.DeepEqual(leaves, want) {
		t.Errorf("PaneLeaves: got %v want %v", leaves, want)
	}
}

func TestSplitPaneNodeNoMatchLeaf(t *testing.T) {
	root := leaf(5)
	// activeIdx=99 doesn't match any leaf — root should be unchanged.
	newRoot := SplitPaneNode(root, 99, 6, SplitV)
	if newRoot.Dir != SplitNone || newRoot.Idx != 5 {
		t.Errorf("non-matching split should leave tree unchanged: %+v", newRoot)
	}
}

// --- ClosePaneNode ---

func TestClosePaneNodeOnlyLeaf(t *testing.T) {
	result := ClosePaneNode(leaf(0), 0)
	if result != nil {
		t.Errorf("closing the only leaf should return nil, got %+v", result)
	}
}

func TestClosePaneNodeLeftChild(t *testing.T) {
	root := split(SplitV, 0.5, leaf(0), leaf(1))
	result := ClosePaneNode(root, 0)
	if result == nil || result.Dir != SplitNone || result.Idx != 1 {
		t.Errorf("closing left should leave right leaf, got %+v", result)
	}
}

func TestClosePaneNodeRightChild(t *testing.T) {
	root := split(SplitV, 0.5, leaf(0), leaf(1))
	result := ClosePaneNode(root, 1)
	if result == nil || result.Dir != SplitNone || result.Idx != 0 {
		t.Errorf("closing right should leave left leaf, got %+v", result)
	}
}

func TestClosePaneNodeNested(t *testing.T) {
	// root: SplitV(0, SplitH(1, 2))
	root := split(SplitV, 0.5, leaf(0), split(SplitH, 0.5, leaf(1), leaf(2)))
	result := ClosePaneNode(root, 2)
	// Result should be SplitV(0, 1)
	if result == nil || result.Dir != SplitV {
		t.Fatalf("expected SplitV root, got %+v", result)
	}
	if result.B == nil || result.B.Dir != SplitNone || result.B.Idx != 1 {
		t.Errorf("right child should be leaf(1), got %+v", result.B)
	}
}

// --- RemapPaneIndices ---

func TestRemapPaneIndices(t *testing.T) {
	root := split(SplitV, 0.5, leaf(1), leaf(2))
	// Remap: 1→0, 2→1 (simulating close of index 0)
	RemapPaneIndices(root, map[int]int{1: 0, 2: 1})
	if root.A.Idx != 0 || root.B.Idx != 1 {
		t.Errorf("after remap: A=%d B=%d, want 0 1", root.A.Idx, root.B.Idx)
	}
}

// --- PaneLeaves / NextPaneIdx ---

func TestPaneLeavesSingleLeaf(t *testing.T) {
	if got := PaneLeaves(leaf(3)); !reflect.DeepEqual(got, []int{3}) {
		t.Errorf("got %v", got)
	}
}

func TestPaneLeavesInOrder(t *testing.T) {
	root := split(SplitV, 0.5, leaf(0), split(SplitH, 0.5, leaf(1), leaf(2)))
	got := PaneLeaves(root)
	want := []int{0, 1, 2}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestNextPaneIdxWraps(t *testing.T) {
	root := split(SplitV, 0.5, leaf(0), split(SplitH, 0.5, leaf(1), leaf(2)))
	if got := NextPaneIdx(root, 0); got != 1 {
		t.Errorf("0→1: got %d", got)
	}
	if got := NextPaneIdx(root, 1); got != 2 {
		t.Errorf("1→2: got %d", got)
	}
	if got := NextPaneIdx(root, 2); got != 0 {
		t.Errorf("2→0 (wrap): got %d", got)
	}
}

func TestNextPaneIdxNoMatch(t *testing.T) {
	root := split(SplitV, 0.5, leaf(0), leaf(1))
	// Active idx not in tree → return first leaf.
	if got := NextPaneIdx(root, 99); got != 0 {
		t.Errorf("no match should return first leaf 0, got %d", got)
	}
}

// --- FocusPaneDir ---

func TestFocusPaneDirVerticalMajor(t *testing.T) {
	// left=0 spans full height; the right column splits top=1 / bottom=2.
	root := split(SplitV, 0.5, leaf(0), split(SplitH, 0.5, leaf(1), leaf(2)))
	b := ComputePaneBounds(root, 0, 0, 81, 25)

	cases := []struct {
		from int
		dir  FocusDir
		want int
	}{
		{0, FocusRight, 1}, // across the vertical divider, tie broken to lowest idx
		{1, FocusLeft, 0},
		{2, FocusLeft, 0},
		{1, FocusDown, 2}, // across the horizontal divider
		{2, FocusUp, 1},
		{0, FocusLeft, -1},  // at the left edge — no wrap
		{1, FocusUp, -1},    // at the top edge
		{2, FocusDown, -1},  // at the bottom edge
		{1, FocusRight, -1}, // at the right edge
	}
	for _, c := range cases {
		if got := FocusPaneDir(b, c.from, c.dir); got != c.want {
			t.Errorf("from %d dir %v = %d, want %d", c.from, c.dir, got, c.want)
		}
	}
}

func TestFocusPaneDirHorizontalMajor(t *testing.T) {
	// top=0 spans full width; the bottom row splits left=1 / right=2.
	root := split(SplitH, 0.5, leaf(0), split(SplitV, 0.5, leaf(1), leaf(2)))
	b := ComputePaneBounds(root, 0, 0, 81, 25)

	cases := []struct {
		from int
		dir  FocusDir
		want int
	}{
		{0, FocusDown, 1},
		{1, FocusRight, 2},
		{2, FocusLeft, 1},
		{1, FocusUp, 0},
		{2, FocusUp, 0},
		{0, FocusUp, -1},
		{1, FocusDown, -1},
		{1, FocusLeft, -1},
		{0, FocusRight, -1},
	}
	for _, c := range cases {
		if got := FocusPaneDir(b, c.from, c.dir); got != c.want {
			t.Errorf("from %d dir %v = %d, want %d", c.from, c.dir, got, c.want)
		}
	}
}

func TestFocusPaneDirGreatestOverlap(t *testing.T) {
	// left column split top=0 (tall) / bottom=1 (short); right=2 spans full
	// height. From 2 moving left must pick the pane with the greater vertical
	// overlap (0), not merely the nearest or lowest-idx one.
	root := split(SplitV, 0.5, split(SplitH, 0.75, leaf(0), leaf(1)), leaf(2))
	b := ComputePaneBounds(root, 0, 0, 81, 25)
	if got := FocusPaneDir(b, 2, FocusLeft); got != 0 {
		t.Errorf("from 2 left should pick the taller overlapping pane 0, got %d", got)
	}
}

func TestFocusPaneDirUnknownActive(t *testing.T) {
	root := split(SplitV, 0.5, leaf(0), leaf(1))
	b := ComputePaneBounds(root, 0, 0, 81, 24)
	if got := FocusPaneDir(b, 99, FocusRight); got != -1 {
		t.Errorf("unknown active pane should return -1, got %d", got)
	}
}

// --- PaneAt ---

func TestPaneAt(t *testing.T) {
	// Two-pane vertical split: left 0 at X[0,40), divider at X=40, right 1 at
	// X[41,81).
	root := split(SplitV, 0.5, leaf(0), leaf(1))
	b := ComputePaneBounds(root, 0, 0, 81, 24)
	cases := []struct {
		x, y int
		want int
	}{
		{10, 5, 0},    // inside the left pane
		{50, 5, 1},    // inside the right pane
		{40, 5, -1},   // on the divider column
		{-1, 5, -1},   // left of everything
		{200, 5, -1},  // beyond the width
		{10, 100, -1}, // below the height
	}
	for _, c := range cases {
		if got := PaneAt(b, c.x, c.y); got != c.want {
			t.Errorf("PaneAt(%d,%d) = %d, want %d", c.x, c.y, got, c.want)
		}
	}
}

// --- Manager integration ---

func TestManagerFocusTermPaneDir(t *testing.T) {
	m := NewManager()
	defer m.CloseAll()

	sleep := []string{"sh", "-c", "sleep 5"}
	ws, err := m.Activate("r/w", t.TempDir(), []Spec{{Kind: Terminal, Argv: sleep}}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}

	// A single pane: directional focus is a no-op.
	m.FocusTermPaneDir("r/w", FocusRight, 80, 24)
	if ws.ActiveTerm != 0 {
		t.Fatalf("single-pane focus should no-op, active=%d", ws.ActiveTerm)
	}

	// Split vertically → panes 0|1 with the new pane (1) focused.
	if _, err := m.SplitTermPane("r/w", t.TempDir(), Spec{Kind: Terminal, Argv: sleep}, SplitV, 80, 24); err != nil {
		t.Fatal(err)
	}
	if ws.ActiveTerm != 1 {
		t.Fatalf("after split active=%d, want 1", ws.ActiveTerm)
	}

	m.FocusTermPaneDir("r/w", FocusLeft, 80, 24)
	if ws.ActiveTerm != 0 {
		t.Fatalf("focus left should select pane 0, got %d", ws.ActiveTerm)
	}
	// Left again at the layout edge → no wrap, no-op.
	m.FocusTermPaneDir("r/w", FocusLeft, 80, 24)
	if ws.ActiveTerm != 0 {
		t.Fatalf("focus left at the edge should no-op, got %d", ws.ActiveTerm)
	}
	m.FocusTermPaneDir("r/w", FocusRight, 80, 24)
	if ws.ActiveTerm != 1 {
		t.Fatalf("focus right should select pane 1, got %d", ws.ActiveTerm)
	}

	// While zoomed, directional focus is suppressed.
	m.ZoomTermPane("r/w") // TermZoomed = true
	m.FocusTermPaneDir("r/w", FocusLeft, 80, 24)
	if ws.ActiveTerm != 1 {
		t.Fatalf("zoomed focus should no-op, got %d", ws.ActiveTerm)
	}
}

func TestManagerFocusTermPaneAt(t *testing.T) {
	m := NewManager()
	defer m.CloseAll()

	sleep := []string{"sh", "-c", "sleep 5"}
	ws, err := m.Activate("r/w", t.TempDir(), []Spec{{Kind: Terminal, Argv: sleep}}, 81, 24)
	if err != nil {
		t.Fatal(err)
	}

	// Split vertically → panes 0|1 (left|right), the new pane 1 focused.
	if _, err := m.SplitTermPane("r/w", t.TempDir(), Spec{Kind: Terminal, Argv: sleep}, SplitV, 81, 24); err != nil {
		t.Fatal(err)
	}
	if ws.ActiveTerm != 1 {
		t.Fatalf("after split active=%d, want 1", ws.ActiveTerm)
	}

	// Click inside the left (non-active) pane → focus moves to 0.
	m.FocusTermPaneAt("r/w", 10, 5, 81, 24)
	if ws.ActiveTerm != 0 {
		t.Fatalf("click in left pane should focus 0, got %d", ws.ActiveTerm)
	}

	// Click on the divider column → no-op.
	m.FocusTermPaneAt("r/w", 40, 5, 81, 24)
	if ws.ActiveTerm != 0 {
		t.Fatalf("click on divider should no-op, got %d", ws.ActiveTerm)
	}

	// Click inside the right pane → focus moves to 1.
	m.FocusTermPaneAt("r/w", 50, 5, 81, 24)
	if ws.ActiveTerm != 1 {
		t.Fatalf("click in right pane should focus 1, got %d", ws.ActiveTerm)
	}

	// While zoomed, click-to-focus is suppressed.
	m.ZoomTermPane("r/w") // TermZoomed = true
	m.FocusTermPaneAt("r/w", 10, 5, 81, 24)
	if ws.ActiveTerm != 1 {
		t.Fatalf("zoomed click should no-op, got %d", ws.ActiveTerm)
	}
}

func TestManagerSplitAndCloseTermPane(t *testing.T) {
	m := NewManager()
	defer m.CloseAll()

	sleep := []string{"sh", "-c", "sleep 5"}
	specs := []Spec{{Kind: Terminal, Argv: sleep}}
	ws, err := m.Activate("r/w", t.TempDir(), specs, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws.Terms) != 1 || ws.TermLayout == nil {
		t.Fatalf("initial state: %d terms, layout=%v", len(ws.Terms), ws.TermLayout)
	}

	// Split vertically → 2 panes, new one focused.
	dir := t.TempDir()
	if _, err := m.SplitTermPane("r/w", dir, Spec{Kind: Terminal, Argv: sleep}, SplitV, 80, 24); err != nil {
		t.Fatal(err)
	}
	if len(ws.Terms) != 2 || ws.ActiveTerm != 1 {
		t.Fatalf("after split: %d terms active=%d, want 2 active=1", len(ws.Terms), ws.ActiveTerm)
	}

	// Cycle → wraps back to 0.
	m.CycleTermPane("r/w")
	if ws.ActiveTerm != 0 {
		t.Fatalf("cycle: active=%d, want 0", ws.ActiveTerm)
	}

	// Zoom toggle.
	m.ZoomTermPane("r/w")
	if !ws.TermZoomed {
		t.Error("zoom should set TermZoomed")
	}
	m.ZoomTermPane("r/w")
	if ws.TermZoomed {
		t.Error("second zoom should clear TermZoomed")
	}

	// Close active (idx 0) → 1 pane left, active clamps to 0.
	if err := m.CloseTermPane("r/w"); err != nil {
		t.Fatal(err)
	}
	if len(ws.Terms) != 1 || ws.ActiveTerm != 0 {
		t.Fatalf("after close: %d terms active=%d, want 1 active=0", len(ws.Terms), ws.ActiveTerm)
	}
	if ws.TermLayout == nil || ws.TermLayout.Dir != SplitNone {
		t.Fatalf("after close: expected single leaf, got %+v", ws.TermLayout)
	}

	// Close last pane → Terms empty, layout nil.
	if err := m.CloseTermPane("r/w"); err != nil {
		t.Fatal(err)
	}
	if len(ws.Terms) != 0 || ws.TermLayout != nil {
		t.Fatalf("after final close: %d terms layout=%v", len(ws.Terms), ws.TermLayout)
	}
}
