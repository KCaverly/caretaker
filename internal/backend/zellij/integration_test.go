package zellij

import (
	"os"
	"os/exec"
	"testing"

	"github.com/KCaverly/caretaker/internal/workspace"
)

// TestZellijLifecycle drives Ensure → Exists → AddSession → Archive against a
// real (headless) zellij. Gated behind CT_ZELLIJ_TEST=1 so it doesn't spawn
// sessions during ordinary `go test` runs.
func TestZellijLifecycle(t *testing.T) {
	if os.Getenv("CT_ZELLIJ_TEST") != "1" {
		t.Skip("set CT_ZELLIJ_TEST=1 to run the zellij integration test")
	}
	if _, err := exec.LookPath("zellij"); err != nil {
		t.Skip("zellij not installed")
	}

	be, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ws := workspace.Default("ctIntegration", "probe", t.TempDir(),
		workspace.Commands{Editor: "true", Agent: "true", Shell: "/bin/sh"})

	// Clean slate, and clean up afterwards.
	_ = be.Archive(ws)
	t.Cleanup(func() { _ = be.Archive(ws) })

	if live, _ := be.Exists(ws); live {
		t.Fatal("session should not exist before Ensure")
	}

	if err := be.Ensure(ws); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	live, err := be.Exists(ws)
	if err != nil {
		t.Fatal(err)
	}
	if !live {
		t.Fatal("session should be live after Ensure")
	}

	if err := be.AddSession(ws, workspace.AgentSession(ws2cmds())); err != nil {
		t.Fatalf("AddSession: %v", err)
	}

	if err := be.Archive(ws); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if live, _ := be.Exists(ws); live {
		t.Fatal("session should be gone after Archive")
	}
}

func ws2cmds() workspace.Commands {
	return workspace.Commands{Editor: "true", Agent: "true", Shell: "/bin/sh"}
}
