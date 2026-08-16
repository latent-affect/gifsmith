//go:build ignore

// Moved here, not deleted, when subtitle-file upload was removed. See
// subtitle.go's matching header in this same retained directory.

package subtitle

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, s string) *Result {
	t.Helper()
	r, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	return r
}

func TestBasicSRT(t *testing.T) {
	r := mustParse(t, "1\n00:00:01,000 --> 00:00:02,500\nHello world\n\n2\n00:00:03,000 --> 00:00:04,000\nSecond line\nwrapped\n")
	if r.Format != "srt" {
		t.Errorf("format = %q, want srt", r.Format)
	}
	if len(r.Cues) != 2 {
		t.Fatalf("got %d cues, want 2", len(r.Cues))
	}
	c := r.Cues[0]
	if c.Start != 1.0 || c.End != 2.5 || c.Text != "Hello world" {
		t.Errorf("cue 0 = %+v", c)
	}
	if r.Cues[1].Text != "Second line\nwrapped" {
		t.Errorf("cue 1 text = %q", r.Cues[1].Text)
	}
}

func TestBOMAndCRLF(t *testing.T) {
	r := mustParse(t, "\uFEFF1\r\n00:00:01,000 --> 00:00:02,000\r\nBOM CRLF cue\r\n\r\n")
	if len(r.Cues) != 1 || r.Cues[0].Text != "BOM CRLF cue" {
		t.Fatalf("cues = %+v", r.Cues)
	}
}

func TestDotMillisecondsInSRT(t *testing.T) {
	r := mustParse(t, "1\n00:00:01.250 --> 00:00:02.750\nDot separator\n")
	if r.Cues[0].Start != 1.25 || r.Cues[0].End != 2.75 {
		t.Errorf("times = %v %v", r.Cues[0].Start, r.Cues[0].End)
	}
}

func TestMissingAndBrokenIndices(t *testing.T) {
	// No counters at all, plus a duplicated counter elsewhere: both fine.
	r := mustParse(t, "00:00:01,000 --> 00:00:02,000\nNo counter\n\n7\n00:00:03,000 --> 00:00:04,000\nCounter says seven\n")
	if len(r.Cues) != 2 {
		t.Fatalf("got %d cues, want 2", len(r.Cues))
	}
	if r.Cues[0].Index != 1 || r.Cues[1].Index != 2 {
		t.Errorf("indices not renumbered: %+v", r.Cues)
	}
}

func TestTagStripping(t *testing.T) {
	r := mustParse(t, "1\n00:00:01,000 --> 00:00:02,000\n<i>Italic</i> and <font color=\"#ff0000\">red</font> &amp; more\n")
	if got := r.Cues[0].Text; got != "Italic and red & more" {
		t.Errorf("text = %q", got)
	}
}

func TestVTT(t *testing.T) {
	src := `WEBVTT
Kind: captions
Language: en

NOTE this is a comment
spanning lines

intro
00:01.000 --> 00:04.000 line:0 position:20% align:start
<v Roger>Hey there

00:00:05.000 --> 00:00:07.500
Second <c.highlight>cue</c>
`
	r := mustParse(t, src)
	if r.Format != "vtt" {
		t.Errorf("format = %q", r.Format)
	}
	if len(r.Cues) != 2 {
		t.Fatalf("got %d cues: %+v", len(r.Cues), r.Cues)
	}
	if r.Cues[0].Start != 1.0 || r.Cues[0].End != 4.0 || r.Cues[0].Text != "Hey there" {
		t.Errorf("cue 0 = %+v", r.Cues[0])
	}
	if r.Cues[1].Text != "Second cue" {
		t.Errorf("cue 1 = %+v", r.Cues[1])
	}
}

func TestOverlapWarning(t *testing.T) {
	r := mustParse(t, "1\n00:00:01,000 --> 00:00:05,000\nA\n\n2\n00:00:03,000 --> 00:00:06,000\nB\n")
	found := false
	for _, w := range r.Warnings {
		if strings.Contains(w, "overlap") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected overlap warning, got %v", r.Warnings)
	}
}

func TestOutOfOrderCuesSorted(t *testing.T) {
	r := mustParse(t, "1\n00:00:10,000 --> 00:00:11,000\nLater\n\n2\n00:00:01,000 --> 00:00:02,000\nEarlier\n")
	if r.Cues[0].Text != "Earlier" || r.Cues[1].Text != "Later" {
		t.Errorf("cues not sorted: %+v", r.Cues)
	}
}

func TestEndBeforeStartSkipped(t *testing.T) {
	r := mustParse(t, "1\n00:00:05,000 --> 00:00:04,000\nBad\n\n2\n00:00:06,000 --> 00:00:07,000\nGood\n")
	if len(r.Cues) != 1 || r.Cues[0].Text != "Good" {
		t.Errorf("cues = %+v", r.Cues)
	}
}

func TestWindows1252Fallback(t *testing.T) {
	// "café" with é as 0xE9 (Latin-1/CP1252), invalid as UTF-8.
	raw := append([]byte("1\n00:00:01,000 --> 00:00:02,000\ncaf"), 0xE9, '\n')
	r, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.Cues[0].Text != "café" {
		t.Errorf("text = %q", r.Cues[0].Text)
	}
	if len(r.Warnings) == 0 {
		t.Errorf("expected encoding warning")
	}
}

func TestUTF16LE(t *testing.T) {
	s := "1\n00:00:01,000 --> 00:00:02,000\nwide\n"
	b := []byte{0xFF, 0xFE}
	for _, r := range s {
		b = append(b, byte(r), 0)
	}
	res, err := Parse(b)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.Cues[0].Text != "wide" {
		t.Errorf("text = %q", res.Cues[0].Text)
	}
}

func TestSRTPositionExtensionDropped(t *testing.T) {
	r := mustParse(t, "1\n00:00:01,000 --> 00:00:02,000\nX1:100 X2:200 Y1:100 Y2:200\nActual text\n")
	if r.Cues[0].Text != "Actual text" {
		t.Errorf("text = %q", r.Cues[0].Text)
	}
}

func TestEmptyAndGarbage(t *testing.T) {
	if _, err := Parse(nil); err == nil {
		t.Error("empty input should error")
	}
	if _, err := Parse([]byte("this is just some text\nwith no timestamps at all\n")); err == nil {
		t.Error("garbage input should error")
	}
}

func TestVTTShortTimestamps(t *testing.T) {
	r := mustParse(t, "WEBVTT\n\n01:02.500 --> 01:03.000\nshort form\n")
	want := 62.5
	if r.Cues[0].Start != want {
		t.Errorf("start = %v, want %v", r.Cues[0].Start, want)
	}
}

func TestDeterminism(t *testing.T) {
	src := "1\n00:00:01,000 --> 00:00:02,000\nSame\n\n2\n00:00:03,000 --> 00:00:04,000\nAgain\n"
	a := mustParse(t, src)
	b := mustParse(t, src)
	if len(a.Cues) != len(b.Cues) {
		t.Fatal("nondeterministic cue count")
	}
	for i := range a.Cues {
		if a.Cues[i] != b.Cues[i] {
			t.Errorf("cue %d differs between runs", i)
		}
	}
}
