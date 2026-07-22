package stack

import "fmt"

// FinishOptions intentionally matches RestackOptions: finishing is the fully
// landed specialization of the same history-cleanup pipeline.
type FinishOptions = RestackOptions

// FinishResult intentionally matches RestackResult so dry-run and JSON clients
// can consume both cleanup operations uniformly.
type FinishResult = RestackResult

// Finish cleans a fully landed stack. It first obtains the complete dry-run
// plan and refuses any partial stack; a live invocation then executes the
// existing, branch-safe restack pipeline. Restack remains a compatible lower-
// level command, while Finish gives users and automation the precise intent.
func Finish(o FinishOptions) (FinishResult, error) {
	previewOptions := o
	previewOptions.DryRun = true
	preview, err := Restack(previewOptions)
	if err != nil {
		return preview, err
	}
	if preview.Status.Stack.NextAction != "finish" {
		return preview, fmt.Errorf("stack is not fully landed (next action: %s)", preview.Status.Stack.NextAction)
	}
	if o.DryRun {
		return preview, nil
	}
	return Restack(o)
}
