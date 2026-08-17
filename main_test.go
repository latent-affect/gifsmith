package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFindBinaryExeSuffix is a regression test for the bug where a
// project-local bin\ containing exactly what the README's own Windows
// instructions say to put there (ffmpeg.exe, ffprobe.exe, gifski.exe) was
// invisible to findTools -- it only ever checked the bare name. The .exe
// fallback runs unconditionally (not gated on runtime.GOOS), so this is a
// real, non-skipped test on every platform, not just an assertion that
// would only ever run on a Windows CI runner this project doesn't have.
func TestFindBinaryExeSuffix(t *testing.T) {
	dir := t.TempDir()

	// Bare name still wins when present (unchanged Unix behavior).
	barePath := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(barePath, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := findBinary(dir, "ffmpeg"); got != barePath {
		t.Errorf("findBinary with bare name present = %q, want %q", got, barePath)
	}
	if err := os.Remove(barePath); err != nil {
		t.Fatal(err)
	}

	// Only the .exe-suffixed file exists (the Windows case) -- must still
	// resolve, not fall through to "".
	exePath := filepath.Join(dir, "ffmpeg.exe")
	if err := os.WriteFile(exePath, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := findBinary(dir, "ffmpeg"); got != exePath {
		t.Errorf("findBinary with only ffmpeg.exe present = %q, want %q", got, exePath)
	}

	// Neither exists and it's not on $PATH: must return "", not panic or
	// silently match an unrelated directory entry.
	if got := findBinary(dir, "totally-not-a-real-binary-xyz"); got != "" {
		t.Errorf("findBinary for a nonexistent binary = %q, want empty", got)
	}
}
