package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestChunksFromScenesNoCuts(t *testing.T) {
	chunks := chunksFromScenes(nil, 5.0)
	if len(chunks) != 1 || chunks[0].Start != 0 || chunks[0].End != 5.0 {
		t.Fatalf("chunks = %+v, want one [0,5] chunk", chunks)
	}
	if chunks[0].Source != "scene" {
		t.Errorf("source = %q, want %q", chunks[0].Source, "scene")
	}
}

func TestChunksFromScenesSplitsOnCuts(t *testing.T) {
	chunks := chunksFromScenes([]float64{2, 4}, 6.0)
	want := []TranscriptChunk{
		{Source: "scene", Start: 0, End: 2},
		{Source: "scene", Start: 2, End: 4},
		{Source: "scene", Start: 4, End: 6},
	}
	if len(chunks) != len(want) {
		t.Fatalf("chunks = %+v, want %+v", chunks, want)
	}
	for i := range want {
		if chunks[i].Start != want[i].Start || chunks[i].End != want[i].End || chunks[i].Source != want[i].Source {
			t.Errorf("chunk[%d] = %+v, want %+v", i, chunks[i], want[i])
		}
	}
}

func TestChunksFromScenesMergesCloseCuts(t *testing.T) {
	// Cuts at 2.0 and 2.3 are 0.3s apart — under minSceneSec (1.0) — so the
	// second should merge forward into the segment it would have started.
	chunks := chunksFromScenes([]float64{2.0, 2.3, 5.0}, 8.0)
	want := []TranscriptChunk{
		{Start: 0, End: 2.0},
		{Start: 2.0, End: 5.0},
		{Start: 5.0, End: 8.0},
	}
	if len(chunks) != len(want) {
		t.Fatalf("chunks = %+v, want %d segments (2.3 should merge away)", chunks, len(want))
	}
	for i := range want {
		if chunks[i].Start != want[i].Start || chunks[i].End != want[i].End {
			t.Errorf("chunk[%d] = %+v, want %+v", i, chunks[i], want[i])
		}
	}
}

func TestChunksFromScenesKeepsShortFinalSegment(t *testing.T) {
	// A cut at 4.5 leaves only 0.5s to the 5.0 end — shorter than
	// minSceneSec, but it's the clip's own end and must never be dropped.
	chunks := chunksFromScenes([]float64{4.5}, 5.0)
	if len(chunks) != 2 || chunks[1].Start != 4.5 || chunks[1].End != 5.0 {
		t.Fatalf("chunks = %+v, want the short final segment kept", chunks)
	}
}

func TestChunksFromScenesTinyClip(t *testing.T) {
	// A clip shorter than minSceneSec with no cuts must still yield one
	// chunk spanning it, not zero chunks.
	chunks := chunksFromScenes(nil, 0.2)
	if len(chunks) != 1 || chunks[0].Start != 0 || chunks[0].End != 0.2 {
		t.Fatalf("chunks = %+v, want one [0,0.2] chunk", chunks)
	}
}

// makeMultiSceneVideo renders a synthetic clip with three hard color-cut
// scenes (2s each) — the same construction used to empirically verify
// scdet's actual metadata-print output format before writing the parser.
func makeMultiSceneVideo(t *testing.T, tools *Tools, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "scenes.mp4")
	cmd := exec.Command(tools.FFmpeg, "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=red:s=160x120:d=2:r=10",
		"-f", "lavfi", "-i", "color=c=blue:s=160x120:d=2:r=10",
		"-f", "lavfi", "-i", "color=c=green:s=160x120:d=2:r=10",
		"-filter_complex", "[0:v][1:v][2:v]concat=n=3:v=1:a=0[out]",
		"-map", "[out]", "-pix_fmt", "yuv420p", p)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating multi-scene video: %v\n%s", err, out)
	}
	return p
}

func TestDetectScenesIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := testTools(t)
	dir := t.TempDir()
	video := makeMultiSceneVideo(t, tools, dir)

	cuts, err := detectScenes(context.Background(), tools, video)
	if err != nil {
		t.Fatalf("detectScenes: %v", err)
	}
	// Hard color cuts at 2s and 4s; scdet should find both and nothing else.
	if len(cuts) != 2 {
		t.Fatalf("cuts = %v, want 2 cuts near t=2 and t=4", cuts)
	}
	if cuts[0] < 1.8 || cuts[0] > 2.2 {
		t.Errorf("cuts[0] = %v, want ~2.0", cuts[0])
	}
	if cuts[1] < 3.8 || cuts[1] > 4.2 {
		t.Errorf("cuts[1] = %v, want ~4.0", cuts[1])
	}
}
