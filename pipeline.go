package main

// pipeline.go — construction and execution of the media pipeline.
//
// Two encoder paths, both deterministic-by-construction (no ML, no RNG):
//
//   gifski : ffmpeg (decode→trim→fps→scale→pad→overlays) → Y4M on stdout
//            → gifski stdin → GIF.  Highest quality (per-frame palettes,
//            temporal dithering via libimagequant).
//   ffmpeg : same filter graph, then a two-pass palettegen/paletteuse
//            encode (Heckbert median-cut + chosen dither, all deterministic
//            algorithms; see docs/domain-briefing.md).
//
// Security invariants (adversarial-review checklist):
//   - No shell is ever invoked: exec.Command with fixed argv arrays.
//   - User-supplied names never become paths; all files live in a
//     server-generated job directory with server-generated names.
//   - The filter graph is written to a file (-filter_complex_script/
//     -/filter_script) so no cue count can overflow an argv or embed
//     metacharacters into a command line.
//   - Numeric settings are clamped server-side regardless of client checks.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ---------- limits & spec ----------

const (
	MinWidth   = 16
	MaxWidth   = 3840 // "scale to any size" within 4K sanity
	MaxHeight  = 4320
	MaxPixels  = 3840 * 2160 // canvas cap: blocks probe-driven memory blow-ups
	MinFPS     = 1
	MaxFPS     = 50 // browsers clamp GIF frame delays <2cs to 100ms; 50fps is the honest ceiling
	MaxCues    = 1000
	MaxCuePNG  = 8 << 20 // 8 MiB per rendered caption overlay
	MinQuality = 1
	MaxQuality = 100
	MaxClips   = 8 // multi-clip (v1.3): bounded — every clip is an upload + a decode
)

// CueSpec is one caption overlay: a PNG (rendered by the browser at exact
// output-canvas size) plus the time window it is visible.
type CueSpec struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	File  string  `json:"-"` // server-side path, never client-supplied
}

// JobSpec is a validated conversion request.
//
// Multi-clip (v1.3): 1..MaxClips clips, each trimmed independently, scaled
// into a common canvas (clip 0 defines the aspect; later clips letterbox),
// and joined with FFmpeg's concat filter into one continuous timeline.
//
// CUE TIME CONTRACT (changed in v1.3): cue Start/End are OUTPUT-timeline
// seconds — time in the assembled, trimmed GIF, starting at 0. In v1.2 they
// were source-absolute times that Validate rebased against the (single)
// trim window; with N clips "source time" is ambiguous, so the mapping from
// per-clip source time to output time now happens in the frontend, which is
// the only place that knows which clip a caption belongs to. Validate
// clamps to [0, TotalDuration] and drops empty windows.
type JobSpec struct {
	Clips []ClipSpec

	Width     int     // output width px
	FPS       float64 // ≤ 50
	Style     string  // "classic" (overlay on video) or "bar" (padded bar)
	BarPx     int     // extra canvas height for style=bar (even, ≥0)
	BarPos    string  // "top" or "bottom" (where the bar is padded)
	BarColor  string  // "white" or "black"
	Encoder   string  // "gifski" or "ffmpeg"
	Quality   int     // gifski --quality 1..100
	Dither    string  // ffmpeg path: bayer | sierra2_4a | floyd_steinberg | none
	MaxColors int     // ffmpeg path palette size 4..256

	Cues []CueSpec
}

type ClipSpec struct {
	Path      string  // server-side path of uploaded video
	TrimStart float64 // seconds; 0 = from start
	TrimEnd   float64 // seconds; 0 = to end
}

// TotalDuration is the assembled output duration: the sum of every clip's
// trimmed window. Valid only after Validate has clamped the trims.
func (s *JobSpec) TotalDuration() float64 {
	var d float64
	for _, c := range s.Clips {
		d += c.TrimEnd - c.TrimStart
	}
	return d
}

// Validate clamps/checks everything that came from the client.
// probes must be per-clip, in clip order.
func (s *JobSpec) Validate(probes []*ProbeInfo) error {
	if len(s.Clips) < 1 || len(s.Clips) > MaxClips {
		return fmt.Errorf("clip count %d out of range [1,%d]", len(s.Clips), MaxClips)
	}
	if len(probes) != len(s.Clips) {
		return fmt.Errorf("internal error: %d probes for %d clips", len(probes), len(s.Clips))
	}
	for i := range s.Clips {
		c := &s.Clips[i]
		p := probes[i]
		if c.TrimStart < 0 || math.IsNaN(c.TrimStart) || math.IsInf(c.TrimStart, 0) {
			c.TrimStart = 0
		}
		if c.TrimEnd <= 0 || c.TrimEnd > p.Duration || math.IsNaN(c.TrimEnd) || math.IsInf(c.TrimEnd, 0) {
			c.TrimEnd = p.Duration
		}
		if c.TrimEnd <= c.TrimStart {
			return fmt.Errorf("clip %d: trim window is empty (start %.3fs, end %.3fs)", i+1, c.TrimStart, c.TrimEnd)
		}
	}

	if s.Width == 0 {
		s.Width = 480
	}
	if s.Width < MinWidth || s.Width > MaxWidth {
		return fmt.Errorf("width %d out of range [%d,%d]", s.Width, MinWidth, MaxWidth)
	}
	s.Width -= s.Width % 2 // yuv420p needs even dimensions

	if s.FPS == 0 {
		s.FPS = 15
	}
	if s.FPS < MinFPS || s.FPS > MaxFPS || math.IsNaN(s.FPS) {
		return fmt.Errorf("fps %.2f out of range [%d,%d] (GIF delays are centiseconds; browsers clamp <2cs to 100ms)", s.FPS, MinFPS, MaxFPS)
	}

	switch s.Style {
	case "", "classic":
		s.Style = "classic"
		s.BarPx = 0
	case "bar":
		if s.BarPx < 16 {
			s.BarPx = 16
		}
		if s.BarPx > 2000 {
			s.BarPx = 2000
		}
		s.BarPx -= s.BarPx % 2
		switch s.BarPos {
		case "", "top":
			s.BarPos = "top"
		case "bottom":
			s.BarPos = "bottom"
		default:
			return fmt.Errorf("barPos must be top or bottom")
		}
		switch s.BarColor {
		case "", "white":
			s.BarColor = "white"
		case "black":
			s.BarColor = "black"
		default:
			return fmt.Errorf("barColor must be white or black")
		}
	default:
		return fmt.Errorf("style must be classic or bar")
	}

	switch s.Encoder {
	case "", "gifski":
		s.Encoder = "gifski"
	case "ffmpeg":
	default:
		return fmt.Errorf("encoder must be gifski or ffmpeg")
	}

	if s.Quality == 0 {
		s.Quality = 90
	}
	if s.Quality < MinQuality || s.Quality > MaxQuality {
		return fmt.Errorf("quality %d out of range [%d,%d]", s.Quality, MinQuality, MaxQuality)
	}

	switch s.Dither {
	case "":
		s.Dither = "sierra2_4a"
	case "bayer", "sierra2_4a", "floyd_steinberg", "none":
	default:
		return fmt.Errorf("dither must be bayer, sierra2_4a, floyd_steinberg or none")
	}

	if s.MaxColors == 0 {
		s.MaxColors = 256
	}
	if s.MaxColors < 4 || s.MaxColors > 256 {
		return fmt.Errorf("maxColors %d out of range [4,256]", s.MaxColors)
	}

	// Canvas cap: probed source aspect is attacker-influenced; without this a
	// tiny video declaring a 2×65535 aspect could demand a gigapixel scale.
	// Clip 0 defines the canvas; later clips letterbox into it, so only
	// probes[0] participates in geometry.
	if outH := s.OutHeight(probes[0]); outH > MaxHeight || s.Width*outH > MaxPixels {
		return fmt.Errorf("output canvas %d×%d exceeds the %dMP limit — reduce width", s.Width, outH, MaxPixels/1_000_000)
	}

	if len(s.Cues) > MaxCues {
		return fmt.Errorf("too many caption cues (%d; max %d)", len(s.Cues), MaxCues)
	}
	// Cue windows are OUTPUT-timeline times (see the JobSpec contract note):
	// clamp to [0, total assembled duration], drop what falls outside.
	total := s.TotalDuration()
	for i := range s.Cues {
		q := &s.Cues[i]
		if math.IsNaN(q.Start) || math.IsNaN(q.End) || q.End <= q.Start {
			return fmt.Errorf("cue %d has invalid time window", i)
		}
		if q.End <= 0 || q.Start >= total {
			q.Start, q.End = -1, -1 // fully outside the output: mark for drop
			continue
		}
		q.Start = math.Max(q.Start, 0)
		q.End = math.Min(q.End, total)
	}
	// Drop out-of-window and zero-width cues (a cue touching the trim edge
	// rebases to a [x,x] window, which would never render).
	kept := s.Cues[:0]
	for _, q := range s.Cues {
		if q.Start >= 0 && q.End > q.Start {
			kept = append(kept, q)
		}
	}
	s.Cues = kept
	sort.SliceStable(s.Cues, func(i, j int) bool { return s.Cues[i].Start < s.Cues[j].Start })
	return nil
}

// OutHeight computes the final canvas height: video scaled to Width with
// aspect preserved and rounded to even, plus the caption bar if any.
// NOTE: this is round-to-nearest-even-floor, which can differ by 2px from
// ffmpeg's scale=W:-2 (truncate-then-align-up). That is fine — the filter
// graph always passes this explicit H, so server, client (same formula in
// JS) and graph agree; -2 semantics are never used.
func (s *JobSpec) OutHeight(probe *ProbeInfo) int {
	h := scaledEvenHeight(probe.Width, probe.Height, s.Width)
	return h + s.BarPx
}

func scaledEvenHeight(srcW, srcH, dstW int) int {
	if srcW <= 0 || srcH <= 0 {
		return dstW // degenerate; never reached after probe validation
	}
	h := int(math.Round(float64(dstW) * float64(srcH) / float64(srcW)))
	if h < 2 {
		h = 2
	}
	return h - h%2
}

// ---------- probing ----------

type ProbeInfo struct {
	Width    int     // display width (rotation applied)
	Height   int     // display height (rotation applied)
	Duration float64 // seconds
	Codec    string
}

// Probe validates that path is a decodable video and returns stream info.
//
// Rotation (adversarial-review finding): phone videos carry a display
// matrix; ffmpeg auto-rotates before the filter graph and browsers report
// rotated videoWidth/Height. We therefore swap probed dimensions for
// ±90/270° rotations so server geometry matches both.
func Probe(ctx context.Context, ffprobe, path string) (*ProbeInfo, error) {
	cmd := exec.CommandContext(ctx, ffprobe,
		"-v", "error",
		"-protocol_whitelist", "file,pipe",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height,codec_name,duration:stream_side_data=rotation:format=duration",
		"-of", "json", path)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("not a decodable video file")
	}
	var raw struct {
		Streams []struct {
			Width    int    `json:"width"`
			Height   int    `json:"height"`
			Codec    string `json:"codec_name"`
			Duration string `json:"duration"`
			SideData []struct {
				Rotation float64 `json:"rotation"`
			} `json:"side_data_list"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &raw); err != nil || len(raw.Streams) == 0 {
		return nil, fmt.Errorf("no video stream found in file")
	}
	st := raw.Streams[0]
	d, _ := strconv.ParseFloat(raw.Format.Duration, 64)
	if d <= 0 { // some containers/elementary streams only carry stream duration
		d, _ = strconv.ParseFloat(st.Duration, 64)
	}
	p := &ProbeInfo{Width: st.Width, Height: st.Height, Duration: d, Codec: st.Codec}
	for _, sd := range st.SideData {
		r := int(math.Round(sd.Rotation))
		r = ((r % 360) + 360) % 360
		if r == 90 || r == 270 {
			p.Width, p.Height = p.Height, p.Width
		}
	}
	if p.Width <= 0 || p.Height <= 0 {
		return nil, fmt.Errorf("video stream has invalid dimensions")
	}
	if p.Duration <= 0 {
		return nil, fmt.Errorf("could not determine video duration")
	}
	return p, nil
}

// ---------- filter graph ----------

// fitEven fits (srcW,srcH) into (boxW,boxH) preserving aspect, both output
// dimensions even (yuv-safe). Used to letterbox clips 1..N-1 into the
// canvas that clip 0 defined.
func fitEven(srcW, srcH, boxW, boxH int) (int, int) {
	if srcW <= 0 || srcH <= 0 {
		return boxW, boxH
	}
	w := boxW
	h := int(math.Round(float64(w) * float64(srcH) / float64(srcW)))
	if h > boxH {
		h = boxH
		w = int(math.Round(float64(h) * float64(srcW) / float64(srcH)))
		if w > boxW {
			w = boxW
		}
	}
	if w < 2 {
		w = 2
	}
	if h < 2 {
		h = 2
	}
	return w - w%2, h - h%2
}

// buildFilterScript writes the filter_complex graph to a file in dir and
// returns its path. Inputs: 0..N-1 = clips (in order), N..N+M-1 = cue PNGs
// (in s.Cues order).
//
// Graph shape (N clips, M cues):
//
//	[k:v] fps → scale (→ letterbox pad) → format=rgba → setsar=1   [vk]
//	[v0][v1]…concat=n=N:v=1:a=0                                    [cat]
//	[cat] (bar pad) …                                              [base]
//	[base][cN] overlay enable window   [b0] … chained …            [out]
//
// fps runs per input BEFORE concat so the assembled timeline is uniform and
// the overlay enable windows line up with output time t exactly. setsar=1
// per input because concat refuses mismatched sample aspect ratios (phone
// clips are the classic offender). All formats are normalized to rgba
// before concat — concat requires uniform pixel formats across inputs.
func buildFilterScript(dir string, s *JobSpec, probes []*ProbeInfo) (string, error) {
	outW := s.Width
	vidH := scaledEvenHeight(probes[0].Width, probes[0].Height, outW)
	canvasH := vidH + s.BarPx
	n := len(s.Clips)

	var g strings.Builder
	for k := 0; k < n; k++ {
		if k > 0 {
			g.WriteString(";")
		}
		if k == 0 {
			// Clip 0 defines the canvas: exact scale (≤1px aspect nudge —
			// same behavior as v1.2's single-clip path).
			fmt.Fprintf(&g, "[0:v]fps=%s,scale=%d:%d:flags=lanczos:sws_dither=none",
				ffFloat(s.FPS), outW, vidH)
		} else {
			fw, fh := fitEven(probes[k].Width, probes[k].Height, outW, vidH)
			fmt.Fprintf(&g, "[%d:v]fps=%s,scale=%d:%d:flags=lanczos:sws_dither=none",
				k, ffFloat(s.FPS), fw, fh)
			if fw != outW || fh != vidH {
				// Letterbox into the canvas, centered (integer px, computed
				// here — the graph never contains arithmetic expressions).
				fmt.Fprintf(&g, ",pad=%d:%d:%d:%d:color=black", outW, vidH, (outW-fw)/2, (vidH-fh)/2)
			}
		}
		fmt.Fprintf(&g, ",format=rgba,setsar=1[v%d]", k)
	}

	cur := "[v0]"
	if n > 1 {
		g.WriteString(";")
		for k := 0; k < n; k++ {
			fmt.Fprintf(&g, "[v%d]", k)
		}
		fmt.Fprintf(&g, "concat=n=%d:v=1:a=0[cat]", n)
		cur = "[cat]"
	}

	g.WriteString(";" + cur)
	if s.BarPx > 0 {
		y := 0
		if s.BarPos == "top" {
			y = s.BarPx // video pushed down; bar occupies the top
		}
		fmt.Fprintf(&g, "pad=%d:%d:0:%d:color=%s,", outW, canvasH, y, s.BarColor)
	}
	g.WriteString("null[base]")

	cur = "[base]"
	for i, c := range s.Cues {
		in := fmt.Sprintf("[%d:v]", n+i)
		scaled := fmt.Sprintf("[c%d]", i)
		next := fmt.Sprintf("[b%d]", i)
		// Scale each overlay to the canvas (belt-and-braces: the browser
		// renders at canvas size already; ±1px client rounding disappears here).
		fmt.Fprintf(&g, ";%sscale=%d:%d%s", in, outW, canvasH, scaled)
		// Half-open window gte*lt, not between(): between is inclusive at both
		// ends, so adjacent cues [1,2][2,3] would stack on the shared boundary
		// frame (adversarial-review finding, verified with select filter).
		fmt.Fprintf(&g, ";%s%soverlay=0:0:format=auto:enable='gte(t,%s)*lt(t,%s)'%s",
			cur, scaled, ffFloat(c.Start), ffFloat(c.End), next)
		cur = next
	}
	fmt.Fprintf(&g, ";%sformat=yuv420p[out]", cur)

	p := filepath.Join(dir, "filtergraph.txt")
	if err := os.WriteFile(p, []byte(g.String()), 0o600); err != nil {
		return "", err
	}
	return p, nil
}

// ffFloat renders a float for a filter expression: fixed decimals, no
// locale, no exponent — deterministic text for deterministic graphs.
func ffFloat(f float64) string {
	return strconv.FormatFloat(math.Round(f*1000)/1000, 'f', -1, 64)
}

// ---------- execution ----------

// Tools holds resolved binary paths (see main.go findTools).
type Tools struct {
	FFmpeg  string
	FFprobe string
	Gifski  string // may be "" if unavailable; encoder=gifski then rejected

	// Local transcription toolchain (v1.4) — all optional as a set: if any
	// piece is missing the Transcribe tab is disabled, everything else works.
	Whisper      string // whisper-cli binary (whisper.cpp)
	WhisperModel string // ggml model file
	Diarizer     string // sherpa-onnx-offline-speaker-diarization binary
	DiarSegModel string // pyannote-segmentation-3.0 ONNX
	DiarEmbModel string // speaker-embedding ONNX (NeMo TitaNet-small)

	FFmpegVersion string
	GifskiVersion string
}

// HasTranscribe reports whether the full local transcription toolchain
// (both binaries and all three model files) is present.
func (t *Tools) HasTranscribe() bool {
	return t.Whisper != "" && t.WhisperModel != "" &&
		t.Diarizer != "" && t.DiarSegModel != "" && t.DiarEmbModel != ""
}

// Progress is a callback receiving 0..1 fractions.
type Progress func(float64)

// Run executes the pipeline for spec into outPath. It reports progress via
// cb (may be nil) and honors ctx cancellation.
func Run(ctx context.Context, t *Tools, dir string, spec *JobSpec, probes []*ProbeInfo, outPath string, cb Progress) error {
	dur := spec.TotalDuration()

	script, err := buildFilterScript(dir, spec, probes)
	if err != nil {
		return fmt.Errorf("writing filter graph: %w", err)
	}

	// Shared decode-input argv (both encoder paths).
	// -ss/-to as *input* options, per clip: fast keyframe seek + accurate
	// decode-discard, and each input's timestamps restart at 0 — the concat
	// filter then re-stamps segments sequentially, so the overlay enable
	// windows see continuous output time.
	// (Input -to verified against ffmpeg 8.1 in review: exact-duration trims.)
	// -protocol_whitelist: defense-in-depth against crafted containers
	// (HLS/concat-style) dereferencing network or nested resources.
	inputArgs := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-nostats",
		"-protocol_whitelist", "file,pipe"}
	for k, clip := range spec.Clips {
		if clip.TrimStart > 0 {
			inputArgs = append(inputArgs, "-ss", ffFloat(clip.TrimStart))
		}
		if clip.TrimEnd < probes[k].Duration {
			inputArgs = append(inputArgs, "-to", ffFloat(clip.TrimEnd))
		}
		inputArgs = append(inputArgs, "-i", clip.Path)
	}
	for _, c := range spec.Cues {
		inputArgs = append(inputArgs, "-i", c.File)
	}

	switch spec.Encoder {
	case "gifski":
		if t.Gifski == "" {
			return fmt.Errorf("gifski binary not available; run scripts/setup-tools.sh or choose the ffmpeg encoder")
		}
		ffArgs := append(append([]string{}, inputArgs...),
			"-filter_complex_script", script, "-map", "[out]")
		if err := runGifskiPath(ctx, t, spec, ffArgs, dur, outPath, cb); err != nil {
			return err
		}
	case "ffmpeg":
		if err := runPalettePath(ctx, t, spec, dir, script, inputArgs, dur, outPath, cb); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown encoder %q", spec.Encoder)
	}

	// Fail-closed metadata scrub (2026-08-16 redesign; see
	// docs/domain/fail-closed-privacy-design.md and gifmeta.go's own
	// header). The old version was fail-open: a strip error logged to the
	// debug ring buffer and the job still reported success, shipping a
	// file with its "gif.ski" fingerprint intact and no visible warning --
	// exactly the "only find out by looking for it" gap the project's own
	// "nothing traces back to how this was made" claim can't tolerate.
	return scrubOutputMetadataFailClosed(outPath)
}

// scrubOutputMetadataFailClosed retries scrubOutputMetadata (transient I/O
// only -- never the encode above) a bounded number of times, and on total
// failure actively removes outPath before returning an error. Extracted
// from Run so it's independently unit-testable without a real video
// encode: scrubOutputMetadata's read/parse/write/verify path doesn't care
// whether outPath came from gifski or a hand-built fixture.
//
// JobManager.run (jobs.go) already gates ResultPath on JobState==JobDone,
// so a returned error here already makes the file unreachable via the
// API; removing outPath is defense-in-depth on top of that state gate,
// not a substitute for it.
func scrubOutputMetadataFailClosed(outPath string) error {
	const maxAttempts = 3
	var scrubErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		scrubErr = scrubOutputMetadata(outPath)
		if scrubErr == nil {
			return nil
		}
		Debug.Add("pipeline", "metadata scrub attempt %d/%d failed for %s: %v",
			attempt, maxAttempts, outPath, scrubErr)
	}
	_ = os.Remove(outPath) // best-effort; the error return below is what actually fails the job
	return fmt.Errorf("metadata scrub could not be verified after %d attempts, refusing to publish: %w",
		maxAttempts, scrubErr)
}

// scrubOutputMetadata replaces outPath with a verified-clean copy, or
// leaves outPath untouched and returns an error. It never mutates outPath
// in place: the scrubbed result is written to a sibling temp file in the
// same directory, independently re-verified FROM DISK, and only then
// os.Rename'd over outPath -- POSIX-atomic on a same-directory rename, so
// nothing can ever observe a partially-written or partially-scrubbed
// outPath. On any error outPath is left exactly as the encoder produced
// it (fingerprint intact); the caller (Run, above) is responsible for
// treating that as fatal, not for silently accepting the unscrubbed file.
func scrubOutputMetadata(outPath string) error {
	data, err := os.ReadFile(outPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", outPath, err)
	}
	cleaned, _, err := stripGIFComments(data)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", outPath, err)
	}

	tmpPath := outPath + ".scrubbing"
	if err := os.WriteFile(tmpPath, cleaned, 0o600); err != nil {
		return fmt.Errorf("writing scrubbed temp file: %w", err)
	}
	defer os.Remove(tmpPath) // no-op once renamed away; cleans up on any early return

	if err := verifyScrubbedClean(tmpPath); err != nil {
		return fmt.Errorf("verifying scrubbed temp file: %w", err)
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		return fmt.Errorf("publishing scrubbed file: %w", err)
	}
	return nil
}

// verifyScrubbedClean independently re-checks a scrubbed GIF from disk
// before it's allowed to become the published output -- deliberately not
// trusting stripGIFComments's own return values on the in-memory buffer it
// just produced. A bug in the stripper's own logic wouldn't necessarily
// catch itself, and a partial/interrupted write can leave bytes on disk
// that don't match what was in memory; re-reading the actual file closes
// both gaps at once.
//
// Two independent layers, not one -- a single re-parse risks the SAME
// class of bug reproducing itself on a second run of the same algorithm:
//
//  1. Idempotent re-parse: stripGIFComments again, on bytes freshly read
//     from disk. changed==false confirms both that the write persisted
//     correctly and that a fresh structural parse finds no remaining
//     Comment Extension block.
//  2. A genuinely different, dumber check: a literal search for the known
//     fingerprint bytes "gif.ski". Deliberately NOT a raw scan for the
//     0x21 0xFE Comment Extension introducer on its own -- gifmeta.go's
//     own header comment records that exact 2-byte sequence recurring 4-5
//     times per file BY CHANCE inside LZW-compressed pixel data, so a
//     blind scan for it would false-positive on genuinely clean files.
//     "gif.ski" is a specific 7-byte ASCII string; the odds of it
//     appearing by accident in compressed binary data are negligible, so
//     this half can stay genuinely dumb (no GIF-structure parsing at all)
//     without false-triggering the way the shorter marker would.
func verifyScrubbedClean(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading for verification: %w", err)
	}
	if _, changed, err := stripGIFComments(data); err != nil {
		return fmt.Errorf("re-parse error: %w", err)
	} else if changed {
		return fmt.Errorf("re-parse still found a Comment Extension block after scrub")
	}
	if bytes.Contains(data, []byte("gif.ski")) {
		return fmt.Errorf("known fingerprint bytes still present after scrub")
	}
	return nil
}

// runGifskiPath pipes Y4M from ffmpeg into gifski.
func runGifskiPath(ctx context.Context, t *Tools, spec *JobSpec, base []string, dur float64, outPath string, cb Progress) error {
	ffArgs := append(base, "-f", "yuv4mpegpipe", "-strict", "-1", "pipe:1")
	ff := exec.CommandContext(ctx, t.FFmpeg, append(ffArgs[:len(ffArgs):len(ffArgs)], progressArgs()...)...)

	gk := exec.CommandContext(ctx, t.Gifski,
		"--quality", strconv.Itoa(spec.Quality),
		"--no-sort",
		"-o", outPath,
		"-")
	pipe, err := ff.StdoutPipe()
	if err != nil {
		return err
	}
	gk.Stdin = pipe

	ffErr := captureProgress(ff, dur, cb)
	var gkStderr strings.Builder
	gk.Stderr = &gkStderr

	if err := ff.Start(); err != nil {
		return fmt.Errorf("starting ffmpeg: %w", err)
	}
	if err := gk.Start(); err != nil {
		_ = ff.Process.Kill()
		_ = ff.Wait()
		return fmt.Errorf("starting gifski: %w", err)
	}

	// Deadlock guard (found in adversarial review): the parent holds a read
	// end of ffmpeg's stdout pipe, so if gifski dies early ffmpeg never sees
	// EPIPE and blocks forever on write — and ff.Wait() would block forever
	// with it. Wait on both concurrently; if gifski exits first (always an
	// error case — it only exits early on failure), kill ffmpeg.
	ffDone := make(chan error, 1)
	gkDone := make(chan error, 1)
	go func() { ffDone <- ff.Wait() }()
	go func() { gkDone <- gk.Wait() }()

	var ffWaitErr, gkWaitErr error
	select {
	case gkWaitErr = <-gkDone:
		if gkWaitErr != nil {
			_ = ff.Process.Kill()
		}
		ffWaitErr = <-ffDone
	case ffWaitErr = <-ffDone:
		gkWaitErr = <-gkDone
	}

	if gkWaitErr != nil {
		return fmt.Errorf("gifski failed: %s", firstLine(gkStderr.String()))
	}
	if ffWaitErr != nil {
		return fmt.Errorf("ffmpeg failed: %s", firstLine(ffErr.String()))
	}
	if cb != nil {
		cb(1)
	}
	return nil
}

// runPalettePath is the two-pass FFmpeg palettegen/paletteuse encoder.
// Both passes share the decode/filter prefix; each pass appends its own
// tail to the base filter graph in a per-pass script file.
func runPalettePath(ctx context.Context, t *Tools, spec *JobSpec, dir, script string, inputArgs []string, dur float64, outPath string, cb Progress) error {
	palette := filepath.Join(dir, "palette.png")
	graph, err := os.ReadFile(script)
	if err != nil {
		return fmt.Errorf("reading filter graph: %w", err)
	}
	base := strings.TrimSpace(string(graph)) // ends in "[out]"

	writePass := func(name, tail string) (string, error) {
		p := filepath.Join(dir, name)
		return p, os.WriteFile(p, []byte(base+";"+tail), 0o600)
	}

	// Pass 1: palette. reserve_transparent=0 — output GIF is fully opaque.
	s1, err := writePass("filtergraph.pass1.txt",
		fmt.Sprintf("[out]palettegen=max_colors=%d:reserve_transparent=0:stats_mode=full[pal]", spec.MaxColors))
	if err != nil {
		return err
	}
	p1 := append(append([]string{}, inputArgs...),
		"-filter_complex_script", s1, "-map", "[pal]",
		"-frames:v", "1", "-update", "1", "-y", palette)
	cmd1 := exec.CommandContext(ctx, t.FFmpeg, append(p1, progressArgs()...)...)
	err1 := captureProgress(cmd1, dur, scaleCB(cb, 0, 0.5))
	if err := cmd1.Run(); err != nil {
		return fmt.Errorf("palette pass failed: %s", firstLine(err1.String()))
	}

	// Pass 2: map through the palette with the chosen (deterministic) dither.
	// Inputs: 0..N-1=clips, N..N+M-1=cues, N+M=palette.
	dither := spec.Dither
	if dither == "bayer" {
		dither = "bayer:bayer_scale=3"
	}
	palIdx := len(spec.Clips) + len(spec.Cues)
	s2, err := writePass("filtergraph.pass2.txt",
		fmt.Sprintf("[out][%d:v]paletteuse=dither=%s[final]", palIdx, dither))
	if err != nil {
		return err
	}
	p2 := append(append([]string{}, inputArgs...), "-i", palette,
		"-filter_complex_script", s2, "-map", "[final]",
		"-loop", "0", "-y", outPath)
	cmd2 := exec.CommandContext(ctx, t.FFmpeg, append(p2, progressArgs()...)...)
	err2 := captureProgress(cmd2, dur, scaleCB(cb, 0.5, 1))
	if err := cmd2.Run(); err != nil {
		return fmt.Errorf("encode pass failed: %s", firstLine(err2.String()))
	}
	if cb != nil {
		cb(1)
	}
	return nil
}

// ---------- progress ----------

func progressArgs() []string {
	return []string{"-progress", "pipe:2"}
}

// progressWriter is an io.Writer for ffmpeg's stderr: it accumulates raw
// output for error reporting (bounded) and parses "-progress" key=value
// lines incrementally — no goroutines, no pipes to leak.
type progressWriter struct {
	raw     strings.Builder
	line    []byte
	total   float64
	cb      Progress
	maxKeep int
}

func newProgressWriter(totalDur float64, cb Progress) *progressWriter {
	return &progressWriter{total: totalDur, cb: cb, maxKeep: 16 * 1024}
}

func (w *progressWriter) Write(p []byte) (int, error) {
	if w.raw.Len() < w.maxKeep {
		n := len(p)
		if w.raw.Len()+n > w.maxKeep {
			n = w.maxKeep - w.raw.Len()
		}
		w.raw.Write(p[:n])
	}
	for _, b := range p {
		if b == '\n' {
			w.handleLine(string(w.line))
			w.line = w.line[:0]
		} else if len(w.line) < 4096 {
			w.line = append(w.line, b)
		}
	}
	return len(p), nil
}

func (w *progressWriter) handleLine(line string) {
	if w.cb == nil || w.total <= 0 {
		return
	}
	if v, ok := strings.CutPrefix(line, "out_time_us="); ok {
		if us, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			f := float64(us) / 1e6 / w.total
			if f > 1 {
				f = 1
			}
			if f >= 0 {
				w.cb(f)
			}
		}
	}
}

func (w *progressWriter) String() string { return w.raw.String() }

// captureProgress attaches a progressWriter to cmd's stderr.
func captureProgress(cmd *exec.Cmd, totalDur float64, cb Progress) *progressWriter {
	w := newProgressWriter(totalDur, cb)
	cmd.Stderr = w
	return w
}

func scaleCB(cb Progress, lo, hi float64) Progress {
	if cb == nil {
		return nil
	}
	return func(f float64) { cb(lo + f*(hi-lo)) }
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	// -progress noise shares stderr; find the first line that isn't key=value.
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.Contains(ln, "=") && !strings.Contains(ln, " ") {
			continue
		}
		return clipStr(ln, 300)
	}
	return clipStr(s, 300)
}

func clipStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
