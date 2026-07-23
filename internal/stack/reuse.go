package stack

import "fmt"

type ReuseOptions = RestackOptions
type ReuseResult = RestackResult

// Reuse clears a fully landed stack while retaining its worktree and local
// branch. It is the explicit alternative to archiving a complete stack.
func Reuse(o ReuseOptions) (ReuseResult, error) {
	previewOptions := o
	previewOptions.DryRun = true
	preview, err := Restack(previewOptions)
	if err != nil {
		return preview, err
	}
	if preview.Status.Stack.NextAction != "complete" {
		return preview, fmt.Errorf("stack is not complete (next action: %s)", preview.Status.Stack.NextAction)
	}
	if o.DryRun {
		return preview, nil
	}
	return Restack(o)
}
