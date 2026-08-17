package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// minimalGIF builds a well-formed 1x1 GIF89a, optionally inserting a
// Comment Extension ("hello") and an Application Extension ("TESTAPP1.0")
// before the image data, so tests can check the walker handles every
// extension type it needs to (drop one kind, preserve the other).
func minimalGIF(withComment, withAppExt bool) []byte {
	var b bytes.Buffer
	b.WriteString("GIF89a")
	b.Write([]byte{0x01, 0x00, 0x01, 0x00, 0x80, 0x00, 0x00}) // 1x1, 2-color GCT, no bg/aspect
	b.Write([]byte{0x00, 0x00, 0x00, 0xFF, 0xFF, 0xFF})       // GCT: black, white

	if withAppExt {
		b.Write([]byte{0x21, 0xFF, 0x0A}) // blockSize=10, matching "TESTAPP1.0" below
		b.WriteString("TESTAPP1.0")
		b.Write([]byte{0x03, 'a', 'b', 'c', 0x00}) // one 3-byte sub-block + terminator
	}
	if withComment {
		b.Write([]byte{0x21, 0xFE, 0x05})
		b.WriteString("hello")
		b.Write([]byte{0x00})
	}
	b.Write([]byte{0x21, 0xF9, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00}) // Graphic Control Extension
	b.Write([]byte{0x2C, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00})
	b.Write([]byte{0x02, 0x02, 0x44, 0x01, 0x00}) // LZW min code size + one sub-block + terminator
	b.WriteByte(0x3B)                             // Trailer
	return b.Bytes()
}

func TestStripGIFCommentsRemovesComment(t *testing.T) {
	with := minimalGIF(true, true)
	without := minimalGIF(false, true)

	out, changed, err := stripGIFComments(with)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatal("changed=false, want true")
	}
	if !bytes.Equal(out, without) {
		t.Fatalf("stripped output does not match the no-comment original\ngot:  % x\nwant: % x", out, without)
	}
	if bytes.Contains(out, []byte("hello")) {
		t.Fatal("comment text still present after strip")
	}
	if !bytes.Contains(out, []byte("TESTAPP1.0")) {
		t.Fatal("unrelated Application Extension was dropped, should have been preserved")
	}
}

func TestStripGIFCommentsNoCommentUnchanged(t *testing.T) {
	without := minimalGIF(false, false)
	out, changed, err := stripGIFComments(without)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Fatal("changed=true for a GIF with no comment extension")
	}
	if !bytes.Equal(out, without) {
		t.Fatal("output differs from input when nothing should have changed")
	}
}

func TestStripGIFCommentsRejectsGarbage(t *testing.T) {
	garbage := []byte("not a gif at all, just some bytes")
	out, changed, err := stripGIFComments(garbage)
	if err == nil {
		t.Fatal("expected an error for non-GIF input")
	}
	if changed {
		t.Fatal("changed=true on a parse failure")
	}
	if !bytes.Equal(out, garbage) {
		t.Fatal("input bytes were altered despite a parse failure")
	}
}

// TestStripGIFCommentsAgainstRealEncoderOutput drives the actual pipeline
// (gifski encoder) end to end and checks the finished file genuinely has no
// gif.ski tag left in it — the whole point of scrubOutputMetadata, checked
// against the real binary rather than just the hand-built unit tests above.
func TestStripGIFCommentsAgainstRealEncoderOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := testTools(t)
	if tools.Gifski == "" {
		t.Skip("gifski not installed")
	}
	dir := t.TempDir()
	video := makeSampleVideo(t, tools, dir)
	spec := baseSpec(video, nil)
	out := dir + "/out.gif"
	runOnce(t, tools, spec, video, out)

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "gif.ski") {
		t.Fatal("finished GIF still contains the gifski \"gif.ski\" comment tag")
	}
}
