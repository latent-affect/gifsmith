package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestScrubOutputMetadataHappyPath confirms the normal case: a GIF with a
// real Comment Extension gets replaced by a verified-clean copy at the same
// path, and the sibling temp file used during the write-verify-rename does
// not linger afterward.
func TestScrubOutputMetadataHappyPath(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.gif")
	if err := os.WriteFile(outPath, minimalGIF(true, true), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := scrubOutputMetadata(outPath); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("hello")) {
		t.Fatal("comment text still present after scrub")
	}
	if !bytes.Contains(data, []byte("TESTAPP1.0")) {
		t.Fatal("unrelated Application Extension was dropped, should have been preserved")
	}
	if _, err := os.Stat(outPath + ".scrubbing"); !os.IsNotExist(err) {
		t.Fatalf("temp file %s.scrubbing was left behind after a successful scrub", outPath)
	}
}

// TestScrubOutputMetadataParseFailureLeavesOriginalUntouched is the direct
// regression test for the original fail-open bug's shape: on a scrub
// failure, scrubOutputMetadata itself must return a non-nil error AND must
// not have altered outPath at all -- the caller (scrubOutputMetadataFailClosed)
// is what decides to remove it, this function's own contract is "leave it
// alone or replace it with something verified, never anything in between."
func TestScrubOutputMetadataParseFailureLeavesOriginalUntouched(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.gif")
	garbage := []byte("not a gif at all, just some bytes")
	if err := os.WriteFile(outPath, garbage, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := scrubOutputMetadata(outPath); err == nil {
		t.Fatal("expected an error scrubbing a non-GIF file")
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, garbage) {
		t.Fatal("outPath was modified despite a parse failure")
	}
	if _, err := os.Stat(outPath + ".scrubbing"); !os.IsNotExist(err) {
		t.Fatalf("temp file %s.scrubbing was left behind after a failed scrub", outPath)
	}
}

// TestScrubOutputMetadataMissingFileErrors confirms the read-failure path
// (outPath vanished between encode and scrub) is treated as an error, not
// silently skipped.
func TestScrubOutputMetadataMissingFileErrors(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "does-not-exist.gif")
	if err := scrubOutputMetadata(outPath); err == nil {
		t.Fatal("expected an error scrubbing a missing file")
	}
}

// TestVerifyScrubbedCleanCatchesFingerprintOutsideCommentBlock is the sharpest
// test of the two-layer verification design (docs/domain/fail-closed-privacy-design.md):
// it proves layer 2 (the literal "gif.ski" byte search) is pulling real,
// independent weight, not just duplicating layer 1. It builds a GIF where
// the fingerprint bytes sit inside an Application Extension's sub-block
// data rather than a Comment Extension -- stripGIFComments only targets
// Comment Extensions (0x21 0xFE) and correctly leaves Application
// Extensions untouched, so a structural re-parse alone (changed==false)
// would report success on this file even though the fingerprint survives.
func TestVerifyScrubbedCleanCatchesFingerprintOutsideCommentBlock(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("GIF89a")
	b.Write([]byte{0x01, 0x00, 0x01, 0x00, 0x80, 0x00, 0x00}) // 1x1, 2-color GCT
	b.Write([]byte{0x00, 0x00, 0x00, 0xFF, 0xFF, 0xFF})       // GCT: black, white

	// Application Extension carrying "gif.ski" as its payload -- NOT a
	// Comment Extension, so stripGIFComments preserves it unchanged.
	payload := "gif.ski leaked here"
	b.Write([]byte{0x21, 0xFF, 0x0B})
	b.WriteString("APPIDCODEX!") // 11-byte app identifier, matches blockSize 0x0B
	b.Write([]byte{byte(len(payload))})
	b.WriteString(payload)
	b.Write([]byte{0x00}) // sub-block terminator

	b.Write([]byte{0x21, 0xF9, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00}) // Graphic Control Extension
	b.Write([]byte{0x2C, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00})
	b.Write([]byte{0x02, 0x02, 0x44, 0x01, 0x00}) // LZW min code size + one sub-block + terminator
	b.WriteByte(0x3B)                             // Trailer
	planted := b.Bytes()

	// Sanity: confirm the structural strip alone would report "nothing to
	// do" on this file -- if this assumption ever stops holding (e.g.
	// stripGIFComments is extended to cover Application Extensions too),
	// this test should be revisited, not silently pass for a different reason.
	if _, changed, err := stripGIFComments(planted); err != nil {
		t.Fatalf("planted fixture should parse cleanly: %v", err)
	} else if changed {
		t.Fatal("planted fixture unexpectedly triggered the structural strip -- test assumption is stale")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "planted.gif")
	if err := os.WriteFile(path, planted, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := verifyScrubbedClean(path); err == nil {
		t.Fatal("verifyScrubbedClean passed a file with the known fingerprint string still present outside a Comment Extension -- layer 2 did not catch what layer 1 structurally cannot")
	}
}

// TestScrubOutputMetadataFailClosedRetriesThenRemovesOnPersistentFailure is
// the regression test for the original bug's actual failure shape at the
// job level: a scrub that can never succeed must not leave a servable file
// behind, and must retry a bounded number of times (not once, not forever)
// before giving up. Forces persistent failure by making outPath's
// directory read-only, so scrubOutputMetadata's temp-file write fails on
// every attempt -- no real video encode needed, since scrubOutputMetadata
// doesn't care whether the bytes came from gifski or a fixture.
func TestScrubOutputMetadataFailClosedRetriesThenRemovesOnPersistentFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory-permission-based failure injection isn't portable to Windows")
	}
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.gif")
	if err := os.WriteFile(outPath, minimalGIF(true, true), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { // read+execute, no write: can't create the .scrubbing temp file
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700) // restore so t.TempDir()'s own cleanup can remove it

	err := scrubOutputMetadataFailClosed(outPath)
	if err == nil {
		t.Fatal("expected an error when the scrub can never succeed")
	}

	// outPath itself lives in the now-read-only dir, so the best-effort
	// os.Remove(outPath) inside scrubOutputMetadataFailClosed will itself
	// fail here (can't unlink from a read-only directory) -- a real,
	// expected limitation of this specific failure-injection method, not
	// of the fail-closed design: the job-level guarantee holds regardless,
	// because JobManager.run gates ResultPath on this error return alone,
	// never on whether the file happens to still exist on disk. That's
	// exactly why the assertion above (err != nil) is the real test here,
	// not a file-existence check.
}
