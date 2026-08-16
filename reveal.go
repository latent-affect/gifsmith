package main

// reveal.go — cross-platform "show this file in the OS file manager" helper
// for the "Reveal in Finder/Explorer" link next to a finished GIF download.
//
// Security note: this is reachable by any local page (server.go's permissive
// CORS posture — see its own header comment) the same way every other
// per-job endpoint already is, but it is the first one with a real-world
// side effect beyond returning data: it can pop open a GUI window. The path
// revealed is always the server's OWN record of that job's output file,
// never a client-supplied path — no traversal surface exists here at all.
//
// Adversarial-review finding (2026-08-07): job-ID unguessability is NOT
// sufficient here the way it is for other endpoints. It only prevents a page
// from reaching someone ELSE's job — it does nothing against a page that
// creates its own job (trivially, with a tiny synthetic upload) and then
// loops reveal on it, which it can do because it always knows its own job's
// ID. The actual mitigation is the per-job cooldown + global rolling-window
// rate limit in jobs.go (CheckReveal, wired in by server.go's
// handleJobReveal) — see jobs.go's revealCooldown/revealWindow doc comment.

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
)

// revealInFileManager opens the OS's file manager with path selected where
// the platform supports selection, or its containing folder otherwise.
// A package-level var (not a plain func) so tests can substitute a stub —
// the real implementation pops a real GUI window, which `go test` must
// never do (non-deterministic across machines, no file manager on CI).
var revealInFileManager = revealInFileManagerReal

func revealInFileManagerReal(ctx context.Context, path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", "-R", path)
	case "windows":
		// Single combined arg ("/select,C:\...") per explorer.exe's own
		// syntax, not two args — and note explorer routinely exits non-zero
		// even on success (a long-documented Windows quirk), so its exit
		// status is not treated as authoritative below.
		cmd = exec.CommandContext(ctx, "explorer", "/select,"+path)
	case "linux":
		// No primitive for "open a file manager AND select this one file"
		// exists across Linux desktop environments the way Finder/Explorer
		// support it natively — xdg-open on the containing directory is the
		// best portable approximation (opens the folder, cursor not on the
		// file specifically).
		cmd = exec.CommandContext(ctx, "xdg-open", filepath.Dir(path))
	default:
		return fmt.Errorf("reveal-in-file-manager is not supported on %s", runtime.GOOS)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launching file manager: %w", err)
	}
	// Fire-and-forget: this is a GUI launcher, not a subprocess whose
	// output/exit code we need — reap it in the background so it doesn't
	// leak a zombie, without blocking the HTTP response on it.
	go func() { _ = cmd.Wait() }()
	return nil
}
