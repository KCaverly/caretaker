package zellij

import (
	"fmt"
	"strings"

	"github.com/KCaverly/caretaker/internal/workspace"
)

// renderLayout produces a zellij KDL layout for a workspace: one tab per
// session, all sharing the worktree directory as their cwd.
func renderLayout(ws workspace.Workspace) string {
	var b strings.Builder
	b.WriteString("layout {\n")
	fmt.Fprintf(&b, "    cwd %s\n", kdlString(ws.Dir))

	for i, s := range ws.Sessions {
		focus := ""
		if i == 0 {
			focus = " focus=true"
		}

		if len(s.Argv) == 0 {
			// Default pane → interactive shell.
			fmt.Fprintf(&b, "    tab name=%s%s\n", kdlString(s.Title), focus)
			continue
		}

		fmt.Fprintf(&b, "    tab name=%s%s {\n", kdlString(s.Title), focus)
		fmt.Fprintf(&b, "        pane command=%s", kdlString(s.Argv[0]))
		if len(s.Argv) > 1 {
			b.WriteString(" {\n            args")
			for _, a := range s.Argv[1:] {
				fmt.Fprintf(&b, " %s", kdlString(a))
			}
			b.WriteString("\n        }\n")
		} else {
			b.WriteString("\n")
		}
		b.WriteString("    }\n")
	}

	b.WriteString("}\n")
	return b.String()
}

// kdlString renders s as a quoted KDL string.
func kdlString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}
