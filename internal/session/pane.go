package session

// SplitDir indicates how a PaneNode splits its space.
type SplitDir int

const (
	SplitNone SplitDir = iota // leaf node — a real terminal session
	SplitV                    // vertical: A is left, B is right
	SplitH                    // horizontal: A is top, B is bottom
)

// PaneNode is one node in the binary split tree describing terminal pane
// layout. Leaves (Dir==SplitNone) reference a session by index in
// Workspace.Terms; internal nodes split their space between two subtrees.
type PaneNode struct {
	Dir   SplitDir
	Ratio float64   // fraction of space given to child A; leaves ignore this
	A, B  *PaneNode // subtrees; nil for leaves
	Idx   int       // index into Workspace.Terms; meaningful only for leaves
}

// PaneBounds is the resolved screen rectangle and session index for one leaf.
type PaneBounds struct {
	X, Y, W, H int
	Idx        int
}

// ComputePaneBounds walks the tree and returns the bounding rectangle for
// every leaf, partitioned within (x, y, w, h). A 1-column divider is reserved
// between SplitV children and a 1-row divider between SplitH children. When
// the available space is too small to split, the first child gets everything.
func ComputePaneBounds(node *PaneNode, x, y, w, h int) []PaneBounds {
	if node == nil {
		return nil
	}
	if node.Dir == SplitNone {
		return []PaneBounds{{X: x, Y: y, W: max(1, w), H: max(1, h), Idx: node.Idx}}
	}
	if node.Dir == SplitV {
		if w < 3 {
			return ComputePaneBounds(node.A, x, y, w, h)
		}
		aW := max(1, int(node.Ratio*float64(w-1)))
		bW := w - aW - 1
		if bW < 1 {
			bW, aW = 1, w-2
		}
		return append(
			ComputePaneBounds(node.A, x, y, aW, h),
			ComputePaneBounds(node.B, x+aW+1, y, bW, h)...,
		)
	}
	// SplitH
	if h < 3 {
		return ComputePaneBounds(node.A, x, y, w, h)
	}
	aH := max(1, int(node.Ratio*float64(h-1)))
	bH := h - aH - 1
	if bH < 1 {
		bH, aH = 1, h-2
	}
	return append(
		ComputePaneBounds(node.A, x, y, w, aH),
		ComputePaneBounds(node.B, x, y+aH+1, w, bH)...,
	)
}

// SplitPaneNode finds the leaf with Idx==activeIdx and replaces it with an
// internal split node whose A child is the original leaf and B child is a new
// leaf pointing to newIdx. Returns the (potentially new) root.
func SplitPaneNode(root *PaneNode, activeIdx, newIdx int, dir SplitDir) *PaneNode {
	if root == nil {
		return &PaneNode{Dir: SplitNone, Idx: newIdx}
	}
	return splitAt(root, activeIdx, newIdx, dir)
}

func splitAt(node *PaneNode, activeIdx, newIdx int, dir SplitDir) *PaneNode {
	if node.Dir == SplitNone {
		if node.Idx != activeIdx {
			return node
		}
		return &PaneNode{
			Dir:   dir,
			Ratio: 0.5,
			A:     &PaneNode{Dir: SplitNone, Idx: activeIdx},
			B:     &PaneNode{Dir: SplitNone, Idx: newIdx},
		}
	}
	result := *node
	result.A = splitAt(node.A, activeIdx, newIdx, dir)
	result.B = splitAt(node.B, activeIdx, newIdx, dir)
	return &result
}

// ClosePaneNode removes the leaf with Idx==closeIdx. If that leaf was the only
// node, nil is returned. After calling this, compact Workspace.Terms and call
// RemapPaneIndices to keep the remaining Idx values consistent.
func ClosePaneNode(root *PaneNode, closeIdx int) *PaneNode {
	if root == nil {
		return nil
	}
	if root.Dir == SplitNone {
		if root.Idx == closeIdx {
			return nil
		}
		return root
	}
	return closeAt(root, closeIdx)
}

func closeAt(node *PaneNode, closeIdx int) *PaneNode {
	if node.Dir == SplitNone {
		return node
	}
	if node.A.Dir == SplitNone && node.A.Idx == closeIdx {
		return node.B
	}
	if node.B.Dir == SplitNone && node.B.Idx == closeIdx {
		return node.A
	}
	result := *node
	result.A = closeAt(node.A, closeIdx)
	result.B = closeAt(node.B, closeIdx)
	return &result
}

// RemapPaneIndices walks every leaf in the tree and applies the old→new index
// mapping. Used after compacting Workspace.Terms to keep Idx values valid.
func RemapPaneIndices(node *PaneNode, mapping map[int]int) {
	if node == nil {
		return
	}
	if node.Dir == SplitNone {
		if newIdx, ok := mapping[node.Idx]; ok {
			node.Idx = newIdx
		}
		return
	}
	RemapPaneIndices(node.A, mapping)
	RemapPaneIndices(node.B, mapping)
}

// PaneLeaves returns the Idx of every leaf in in-order (left-to-right,
// top-to-bottom) traversal order.
func PaneLeaves(root *PaneNode) []int {
	if root == nil {
		return nil
	}
	if root.Dir == SplitNone {
		return []int{root.Idx}
	}
	return append(PaneLeaves(root.A), PaneLeaves(root.B)...)
}

// NextPaneIdx returns the next leaf Idx after activeIdx in the in-order
// traversal, wrapping around.
func NextPaneIdx(root *PaneNode, activeIdx int) int {
	leaves := PaneLeaves(root)
	if len(leaves) == 0 {
		return 0
	}
	for i, idx := range leaves {
		if idx == activeIdx {
			return leaves[(i+1)%len(leaves)]
		}
	}
	return leaves[0]
}

// paneContains reports whether any leaf in the subtree has Idx==target.
func paneContains(node *PaneNode, target int) bool {
	if node == nil {
		return false
	}
	if node.Dir == SplitNone {
		return node.Idx == target
	}
	return paneContains(node.A, target) || paneContains(node.B, target)
}
