package main

// scenedetect.go — scene-change detection fallback for the Transcribe tab.
//
// When a clip has no audio track, there is nothing for whisper.cpp to
// transcribe. Rather than fail the job outright, GIFsmith uses FFmpeg's own
// scdet filter to find camera-cut timestamps and offers them as the same
// kind of timing scaffold real dialogue chunks provide: a blank row per
// detected scene, positioned where a cut actually happens, for the user to
// write their own caption against.
//
// scdet stamps `lavfi.scd.time` frame metadata at each detected cut
// (verified against FFmpeg 8.1.2's own doc/filters.texi, not memory — see
// project memory for the sourcing and the library-scout vetting that ruled
// out an external tool like PySceneDetect in favor of this zero-new-
// dependency approach). The paired `metadata` filter (mode=print) reads
// that back out as plain stdout lines instead of scraping ffmpeg's log:
//
//	frame:20   pts:163840  pts_time:2
//	lavfi.scd.time=2
//
// (empirically confirmed against a synthetic 3-scene test clip, since the
// upstream docs describe the mechanism but not a literal sample line).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

const (
	// sceneThreshold is ffmpeg's own documented default for scdet's
	// threshold option ("good values are in the [8.0, 14.0] range").
	sceneThreshold = 10.0 // CITED: FFmpeg 8.1.2 doc/filters.texi @section scdet: "Default value is 10."

	// minSceneSec merges cuts closer together than this into the following
	// segment — a fast-cut clip (music video, action montage) shouldn't
	// flood the UI with a wall of sub-second chunks.
	minSceneSec = 1.0
)

var scdetTimeRe = regexp.MustCompile(`^lavfi\.scd\.time=([0-9]+(?:\.[0-9]+)?)\s*$`)

// detectScenes returns camera-cut timestamps (clip-local seconds, strictly
// ascending, deduplicated) found by FFmpeg's scdet filter.
func detectScenes(ctx context.Context, t *Tools, videoPath string) ([]float64, error) {
	filter := fmt.Sprintf("scdet=threshold=%s,metadata=print:key=lavfi.scd.time:file=-", ffFloat(sceneThreshold))
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error",
		"-protocol_whitelist", "file,pipe",
		"-i", videoPath,
		"-vf", filter,
		// Discard the actual video output — only the metadata print (stdout)
		// matters. This is a REAL production dependency on the rawvideo
		// muxer+encoder (cross-unit-contract review finding: an earlier
		// version of this comment wrongly claimed rawvideo was "already
		// enabled for the gifski path" — the gifski path actually uses
		// yuv4mpegpipe, pipeline.go's runGifskiPath — so don't let a future
		// enable-list cleanup treat scripts/build-ffmpeg.sh's
		// --enable-muxer/-encoder=rawvideo as test-only and strip it; see
		// that script's own note next to those flags for the full story).
		// The more usual `-f null -` needs its own separate enable, which
		// this avoids by reusing rawvideo instead.
		"-f", "rawvideo", "-y", os.DevNull}
	cmd := exec.CommandContext(ctx, t.FFmpeg, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("scene detection failed: %s", firstLine(stderr.String()))
	}
	var cuts []float64
	for _, line := range strings.Split(stdout.String(), "\n") {
		m := scdetTimeRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			// Unreachable in practice: scdetTimeRe's own capture group only
			// matches valid float syntax. Logged anyway (review finding) —
			// if ffmpeg's scdet/metadata output format ever drifted, cut
			// timestamps would otherwise be dropped with zero trace.
			Debug.Add("transcribe", "detectScenes %s: unparseable cut time %q: %v", videoPath, m[1], err)
			continue
		}
		if len(cuts) == 0 || v > cuts[len(cuts)-1] {
			cuts = append(cuts, v)
		}
	}
	return cuts, nil
}

// chunksFromScenes turns cut timestamps into TranscriptChunk boundaries
// spanning the whole clip: [0,cut1), [cut1,cut2), ..., [cutN,duration]. A
// clip with no detected cuts at all still gets one chunk for its full
// duration — there is always something to write a caption against.
func chunksFromScenes(cuts []float64, duration float64) []TranscriptChunk {
	bounds := append([]float64{0}, cuts...)
	bounds = append(bounds, duration)

	merged := bounds[:1:1]
	for _, b := range bounds[1:] {
		// The final boundary (the clip's own end) is always kept, even if
		// it lands less than minSceneSec after the previous one — there is
		// nowhere further to merge it to.
		if b != duration && b-merged[len(merged)-1] < minSceneSec {
			continue
		}
		merged = append(merged, b)
	}

	out := make([]TranscriptChunk, 0, len(merged)-1)
	for i := 0; i < len(merged)-1; i++ {
		start, end := merged[i], merged[i+1]
		if end <= start {
			continue
		}
		out = append(out, TranscriptChunk{Source: "scene", Start: start, End: end})
	}
	return out
}

// sceneChunksForClip runs the full no-audio fallback for one clip.
func sceneChunksForClip(ctx context.Context, t *Tools, clip TClip) ([]TranscriptChunk, error) {
	cuts, err := detectScenes(ctx, t, clip.Path)
	if err != nil {
		return nil, err
	}
	return chunksFromScenes(cuts, clip.Duration), nil
}
