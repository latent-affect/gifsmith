package main

// Integration tests. They exercise the real ffmpeg/gifski binaries and are
// skipped automatically when those are absent (unit tests still run).
//
//	go test ./...            # everything available in ./bin or $PATH
//	go test -short ./...     # skip integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func testTools(t *testing.T) *Tools {
	t.Helper()
	tools, err := findTools("bin", "")
	if err != nil {
		t.Skipf("toolchain unavailable: %v", err)
	}
	return tools
}

// makeSampleVideo renders a deterministic 4-second test clip.
func makeSampleVideo(t *testing.T, tools *Tools, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "sample.mp4")
	cmd := exec.Command(tools.FFmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=30:duration=4",
		"-pix_fmt", "yuv420p", "-y", p)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating sample video: %v\n%s", err, out)
	}
	return p
}

// makeCuePNG draws a synthetic caption overlay (opaque box in the caption
// area, transparent elsewhere) at exact canvas size.
func makeCuePNG(t *testing.T, dir string, name string, w, h int, c color.RGBA) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := h * 3 / 4; y < h*3/4+h/8; y++ {
		for x := w / 4; x < w*3/4; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	return p
}

func sha(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func baseSpec(video string, cues []CueSpec, encoder string) *JobSpec {
	return &JobSpec{
		Clips:   []ClipSpec{{Path: video, TrimStart: 0.5, TrimEnd: 3.5}},
		Width:   240,
		FPS:     10,
		Style:   "classic",
		Encoder: encoder,
		Quality: 80,
		Cues:    cues,
	}
}

func runOnce(t *testing.T, tools *Tools, spec *JobSpec, video string, out string) {
	t.Helper()
	dir := filepath.Dir(out)
	// Probe every clip in the spec (they may be different files).
	probes := make([]*ProbeInfo, 0, len(spec.Clips))
	for _, c := range spec.Clips {
		probe, err := Probe(context.Background(), tools.FFprobe, c.Path)
		if err != nil {
			t.Fatalf("probe %s: %v", c.Path, err)
		}
		probes = append(probes, probe)
	}
	_ = video
	// Deep-copy the spec: Validate mutates cue windows.
	cp := *spec
	cp.Clips = append([]ClipSpec{}, spec.Clips...)
	cp.Cues = append([]CueSpec{}, spec.Cues...)
	if err := cp.Validate(probes); err != nil {
		t.Fatalf("validate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := Run(ctx, tools, dir, &cp, probes, out, nil); err != nil {
		t.Fatalf("run (%s): %v", spec.Encoder, err)
	}
	fi, err := os.Stat(out)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("no output produced")
	}
	hdr := make([]byte, 6)
	f, _ := os.Open(out)
	_, _ = f.Read(hdr)
	f.Close()
	if string(hdr) != "GIF89a" && string(hdr) != "GIF87a" {
		t.Fatalf("output is not a GIF (header %q)", hdr)
	}
}

func TestPipelineFFmpegEncoderDeterministic(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := testTools(t)
	dir := t.TempDir()
	video := makeSampleVideo(t, tools, dir)
	// Canvas geometry for 240px wide, 320x180 source: h = round(240*180/320)=135 → 134.
	cue1 := makeCuePNG(t, dir, "c1.png", 240, 134, color.RGBA{255, 255, 255, 255})
	cue2 := makeCuePNG(t, dir, "c2.png", 240, 134, color.RGBA{255, 255, 0, 255})
	cues := []CueSpec{
		{Start: 1.0, End: 2.0, File: cue1},
		{Start: 2.0, End: 3.0, File: cue2},
	}

	d1, d2 := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	_ = os.MkdirAll(d1, 0o700)
	_ = os.MkdirAll(d2, 0o700)
	o1, o2 := filepath.Join(d1, "out.gif"), filepath.Join(d2, "out.gif")

	spec := baseSpec(video, cues, "ffmpeg")
	runOnce(t, tools, spec, video, o1)
	runOnce(t, tools, spec, video, o2)
	if sha(t, o1) != sha(t, o2) {
		t.Errorf("ffmpeg palette path is NOT byte-deterministic across runs")
	}
}

func TestPipelineGifskiEncoder(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := testTools(t)
	if tools.Gifski == "" {
		t.Skip("gifski not installed")
	}
	dir := t.TempDir()
	video := makeSampleVideo(t, tools, dir)
	cue := makeCuePNG(t, dir, "c1.png", 240, 134, color.RGBA{255, 255, 255, 255})
	cues := []CueSpec{{Start: 1.0, End: 2.5, File: cue}}

	d1, d2 := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	_ = os.MkdirAll(d1, 0o700)
	_ = os.MkdirAll(d2, 0o700)
	o1, o2 := filepath.Join(d1, "out.gif"), filepath.Join(d2, "out.gif")

	spec := baseSpec(video, cues, "gifski")
	runOnce(t, tools, spec, video, o1)
	runOnce(t, tools, spec, video, o2)
	if sha(t, o1) == sha(t, o2) {
		t.Log("gifski output is byte-deterministic across runs on this machine")
	} else {
		t.Log("NOTE: gifski output differs between runs (threaded encode); " +
			"use the ffmpeg encoder when byte-exact reproducibility matters")
	}
}

// makeSampleVideo2 renders a second deterministic clip with a DIFFERENT
// aspect ratio (portrait-ish 180x240) to exercise the letterbox path.
func makeSampleVideo2(t *testing.T, tools *Tools, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "sample2.mp4")
	cmd := exec.Command(tools.FFmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "smptebars=size=180x240:rate=25:duration=3",
		"-pix_fmt", "yuv420p", "-y", p)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating sample video 2: %v\n%s", err, out)
	}
	return p
}

// gifDuration sums GIF frame delays via ffprobe (frame count / fps is
// unreliable for GIFs; use the container duration).
func gifDuration(t *testing.T, tools *Tools, p string) float64 {
	t.Helper()
	probe, err := Probe(context.Background(), tools.FFprobe, p)
	if err != nil {
		t.Fatalf("probing gif: %v", err)
	}
	return probe.Duration
}

func TestPipelineMultiClipConcat(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := testTools(t)
	dir := t.TempDir()
	v1 := makeSampleVideo(t, tools, dir)  // 320x180 landscape, 4s
	v2 := makeSampleVideo2(t, tools, dir) // 180x240 portrait, 3s

	// Canvas from clip 0: 240x134. A cue straddling the seam (t=2.0 on the
	// assembled timeline) must render across both clips' frames.
	cue := makeCuePNG(t, dir, "seam.png", 240, 134, color.RGBA{0, 255, 0, 255})
	spec := &JobSpec{
		Clips: []ClipSpec{
			{Path: v1, TrimStart: 1, TrimEnd: 3}, // 2s
			{Path: v2, TrimStart: 0, TrimEnd: 2}, // 2s → total 4s
		},
		Width:   240,
		FPS:     10,
		Style:   "classic",
		Encoder: "ffmpeg",
		Dither:  "bayer",
		Cues:    []CueSpec{{Start: 1.5, End: 2.5, File: cue}}, // straddles the seam
	}

	d1, d2 := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	_ = os.MkdirAll(d1, 0o700)
	_ = os.MkdirAll(d2, 0o700)
	o1, o2 := filepath.Join(d1, "out.gif"), filepath.Join(d2, "out.gif")
	runOnce(t, tools, spec, v1, o1)
	runOnce(t, tools, spec, v1, o2)

	// Determinism holds across the concat path too.
	if sha(t, o1) != sha(t, o2) {
		t.Errorf("multi-clip ffmpeg path is NOT byte-deterministic across runs")
	}

	// Assembled duration = sum of trimmed windows (GIF frame timing is
	// centisecond-quantized; allow generous slack).
	if d := gifDuration(t, tools, o1); d < 3.5 || d > 4.5 {
		t.Errorf("assembled gif duration = %.2fs, want ≈4s", d)
	}

	// Geometry comes from clip 0 even though clip 1 is portrait.
	probe, err := Probe(context.Background(), tools.FFprobe, o1)
	if err != nil {
		t.Fatalf("probing output: %v", err)
	}
	if probe.Width != 240 || probe.Height != 134 {
		t.Errorf("multi-clip output = %dx%d, want 240x134 (clip 0 defines the canvas)", probe.Width, probe.Height)
	}
}

func TestFitEven(t *testing.T) {
	cases := []struct{ sw, sh, bw, bh, ww, wh int }{
		{320, 180, 240, 134, 238, 134}, // wide into slightly-wider box: height-bound
		{180, 240, 240, 134, 100, 134}, // portrait into landscape: pillarbox
		{320, 180, 240, 200, 240, 134}, // width-bound
		{100, 100, 240, 134, 134, 134}, // square: height-bound
	}
	for _, c := range cases {
		if w, h := fitEven(c.sw, c.sh, c.bw, c.bh); w != c.ww || h != c.wh {
			t.Errorf("fitEven(%d,%d,%d,%d) = %dx%d, want %dx%d", c.sw, c.sh, c.bw, c.bh, w, h, c.ww, c.wh)
		}
	}
}

func TestPipelineBarStyle(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := testTools(t)
	dir := t.TempDir()
	video := makeSampleVideo(t, tools, dir)
	// Bar canvas: vidH 134 + bar 40 = 174.
	cue := makeCuePNG(t, dir, "c1.png", 240, 174, color.RGBA{0, 0, 0, 255})
	spec := baseSpec(video, []CueSpec{{Start: 1.0, End: 2.0, File: cue}}, "ffmpeg")
	spec.Style = "bar"
	spec.BarPx = 40
	spec.BarPos = "top"
	spec.BarColor = "white"
	out := filepath.Join(dir, "out.gif")
	runOnce(t, tools, spec, video, out)

	// Verify output geometry via ffprobe.
	probe, err := Probe(context.Background(), tools.FFprobe, out)
	if err != nil {
		t.Fatalf("probing output gif: %v", err)
	}
	if probe.Width != 240 || probe.Height != 174 {
		t.Errorf("bar output = %dx%d, want 240x174", probe.Width, probe.Height)
	}
}

func TestValidateRejectsHostileSpecs(t *testing.T) {
	probes := []*ProbeInfo{{Width: 640, Height: 360, Duration: 10}}
	base := func() *JobSpec {
		return &JobSpec{Clips: []ClipSpec{{Path: "x", TrimEnd: 10}}}
	}

	s := base()
	s.Width = 999999
	if err := s.Validate(probes); err == nil {
		t.Error("giant width accepted")
	}
	s = base()
	s.FPS = 1000
	if err := s.Validate(probes); err == nil {
		t.Error("1000fps accepted (browsers cap at 50)")
	}
	s = base()
	s.Style = "banana"
	if err := s.Validate(probes); err == nil {
		t.Error("unknown style accepted")
	}
	s = base()
	s.Encoder = "rm -rf"
	if err := s.Validate(probes); err == nil {
		t.Error("unknown encoder accepted")
	}
	s = base()
	s.Clips[0].TrimStart = 8
	s.Clips[0].TrimEnd = 2
	if err := s.Validate(probes); err == nil {
		t.Error("inverted trim accepted")
	}

	// Canvas cap: a hostile 2×3000 source at width 3840 would demand a
	// ~5.7-gigapixel canvas without the MaxPixels/MaxHeight guard.
	s = base()
	s.Width = 3840
	if err := s.Validate([]*ProbeInfo{{Width: 2, Height: 3000, Duration: 10}}); err == nil {
		t.Error("extreme-aspect canvas accepted (memory blow-up vector)")
	}

	// Multi-clip caps: zero clips and too many clips are both rejected, as is
	// a probe/clip count mismatch (internal invariant).
	s = base()
	s.Clips = nil
	if err := s.Validate(probes); err == nil {
		t.Error("zero clips accepted")
	}
	s = base()
	for i := 0; i < MaxClips; i++ {
		s.Clips = append(s.Clips, ClipSpec{Path: "x", TrimEnd: 10})
	}
	if err := s.Validate(probes); err == nil {
		t.Error("clip/probe count mismatch accepted")
	}
}

// v1.3 cue contract: cue windows are OUTPUT-timeline seconds, clamped to
// [0, total assembled duration].
func TestValidateCueWindowMath(t *testing.T) {
	probes := []*ProbeInfo{{Width: 640, Height: 360, Duration: 10}}
	s := &JobSpec{
		// Trim [2,8] → 6s of output timeline.
		Clips: []ClipSpec{{Path: "x", TrimStart: 2, TrimEnd: 8}},
		Cues: []CueSpec{
			{Start: -3, End: -1, File: "a"}, // fully before output start → dropped
			{Start: -1, End: 1, File: "b"},  // clamped to [0,1]
			{Start: 3, End: 9, File: "c"},   // clamped to [3,6]
			{Start: 6.5, End: 9, File: "d"}, // fully after output end → dropped
			{Start: 2, End: 2.5, File: "e"}, // inside → kept as-is
		},
	}
	if err := s.Validate(probes); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(s.Cues) != 3 {
		t.Fatalf("kept %d cues, want 3: %+v", len(s.Cues), s.Cues)
	}
	if s.Cues[0].Start != 0 || s.Cues[0].End != 1 {
		t.Errorf("cue b window = [%v,%v], want [0,1]", s.Cues[0].Start, s.Cues[0].End)
	}
	if s.Cues[1].Start != 2 || s.Cues[1].End != 2.5 {
		t.Errorf("cue e window = [%v,%v], want [2,2.5]", s.Cues[1].Start, s.Cues[1].End)
	}
	if s.Cues[2].Start != 3 || s.Cues[2].End != 6 {
		t.Errorf("cue c window = [%v,%v], want [3,6]", s.Cues[2].Start, s.Cues[2].End)
	}
}

// Multi-clip: cue windows clamp against the SUM of trimmed durations.
func TestValidateCueWindowMathMultiClip(t *testing.T) {
	probes := []*ProbeInfo{
		{Width: 640, Height: 360, Duration: 10},
		{Width: 320, Height: 240, Duration: 5},
	}
	s := &JobSpec{
		// 3s + 2s of output timeline = 5s total.
		Clips: []ClipSpec{
			{Path: "x", TrimStart: 1, TrimEnd: 4},
			{Path: "y", TrimStart: 0, TrimEnd: 2},
		},
		Cues: []CueSpec{
			{Start: 2.5, End: 4.5, File: "a"}, // straddles the clip seam → kept whole
			{Start: 4, End: 9, File: "b"},     // clamped to [4,5]
			{Start: 5, End: 6, File: "c"},     // at/after output end → dropped
		},
	}
	if err := s.Validate(probes); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if s.TotalDuration() != 5 {
		t.Fatalf("TotalDuration = %v, want 5", s.TotalDuration())
	}
	if len(s.Cues) != 2 {
		t.Fatalf("kept %d cues, want 2: %+v", len(s.Cues), s.Cues)
	}
	if s.Cues[0].Start != 2.5 || s.Cues[0].End != 4.5 {
		t.Errorf("seam-straddling cue = [%v,%v], want [2.5,4.5]", s.Cues[0].Start, s.Cues[0].End)
	}
	if s.Cues[1].Start != 4 || s.Cues[1].End != 5 {
		t.Errorf("cue b = [%v,%v], want [4,5]", s.Cues[1].Start, s.Cues[1].End)
	}
}

// v1.2-shaped requests (no clips[] array) carry SOURCE-ABSOLUTE cue times;
// the server rebases them to the v1.3 output-timeline contract.
func TestRebaseLegacyCues(t *testing.T) {
	got := rebaseLegacyCues([]CueSpec{
		{Start: 0, End: 1, File: "a"},   // fully before trim → dropped
		{Start: 1, End: 3, File: "b"},   // clipped to [2,3] → out [0,1]
		{Start: 5, End: 9, File: "c"},   // clipped to [5,8] → out [3,6]
		{Start: 8.5, End: 9, File: "d"}, // fully after → dropped
		{Start: 1, End: 2, File: "e"},   // touches trim start → [0,0] → dropped
	}, 2, 8, 10)
	if len(got) != 2 {
		t.Fatalf("kept %d cues, want 2: %+v", len(got), got)
	}
	if got[0].Start != 0 || got[0].End != 1 || got[0].File != "b" {
		t.Errorf("cue b = %+v, want out [0,1]", got[0])
	}
	if got[1].Start != 3 || got[1].End != 6 || got[1].File != "c" {
		t.Errorf("cue c = %+v, want out [3,6]", got[1])
	}
	// Hostile trim values fall back to the full duration.
	got = rebaseLegacyCues([]CueSpec{{Start: 4, End: 5, File: "x"}}, math.NaN(), -1, 10)
	if len(got) != 1 || got[0].Start != 4 || got[0].End != 5 {
		t.Errorf("NaN/negative trims should mean full window: %+v", got)
	}
}

func TestScaledEvenHeight(t *testing.T) {
	cases := []struct{ sw, sh, dw, want int }{
		{320, 180, 240, 134}, // 135 → even 134
		{1920, 1080, 480, 270},
		{640, 480, 480, 360},
		{100, 100, 480, 480},
	}
	for _, c := range cases {
		if got := scaledEvenHeight(c.sw, c.sh, c.dw); got != c.want {
			t.Errorf("scaledEvenHeight(%d,%d,%d) = %d, want %d", c.sw, c.sh, c.dw, got, c.want)
		}
	}
}

// TestAPIEndToEnd drives the HTTP API exactly as the frontend does.
func TestAPIEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := testTools(t)
	dir := t.TempDir()
	video := makeSampleVideo(t, tools, dir)

	jm := NewJobManager(filepath.Join(dir, "tmp"), tools)
	srv := NewServer(tools, jm, 512, DefaultWarnMB)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 1. A single cue's timing. Captions come from local transcription (or
	// scene-cut detection for silent clips) now, not subtitle-file upload
	// (removed -- see audible-deletion-flag/subtitle/), so this test only
	// needs one representative (start, end) pair to exercise job submission
	// and rendering below; it never routed through the parser itself.
	cueStart, cueEnd := 1.0, 2.0

	// 2. Submit a job with one rendered cue PNG (240x134 canvas).
	cuePath := makeCuePNG(t, dir, "cue0.png", 240, 134, color.RGBA{255, 0, 0, 255})
	var jb bytes.Buffer
	jw := multipart.NewWriter(&jb)
	settings := map[string]any{
		"trimStart": 0.5, "trimEnd": 3.5, "width": 240, "fps": 10,
		"style": "classic", "encoder": "ffmpeg", "dither": "bayer",
		"cues": []map[string]float64{{"start": cueStart, "end": cueEnd}},
	}
	sj, _ := json.Marshal(settings)
	_ = jw.WriteField("settings", string(sj))
	vw, _ := jw.CreateFormFile("video", "v.mp4")
	vb, _ := os.ReadFile(video)
	_, _ = vw.Write(vb)
	cw, _ := jw.CreateFormFile("cue_0", "cue_0.png")
	cb, _ := os.ReadFile(cuePath)
	_, _ = cw.Write(cb)
	jw.Close()

	resp, err := http.Post(ts.URL+"/api/jobs", jw.FormDataContentType(), &jb)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		var e map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&e)
		t.Fatalf("job create: HTTP %d %v", resp.StatusCode, e)
	}
	var created struct {
		ID            string `json:"id"`
		EstimateBytes int64  `json:"estimateBytes"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.ID == "" || created.EstimateBytes <= 0 {
		t.Fatalf("bad create response: %+v", created)
	}

	// 3. Poll to completion.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if time.Now().After(deadline) {
			t.Fatal("job did not finish in time")
		}
		r, err := http.Get(ts.URL + "/api/jobs/" + created.ID)
		if err != nil {
			t.Fatal(err)
		}
		var st struct {
			State string `json:"state"`
			Error string `json:"error"`
		}
		_ = json.NewDecoder(r.Body).Decode(&st)
		r.Body.Close()
		if st.State == "done" {
			break
		}
		if st.State == "error" {
			t.Fatalf("job failed: %s", st.Error)
		}
		time.Sleep(300 * time.Millisecond)
	}

	// 4. Download and verify the GIF.
	r, err := http.Get(ts.URL + "/api/jobs/" + created.ID + "/result")
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 6)
	if _, err := io.ReadFull(r.Body, body); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if string(body) != "GIF89a" && string(body) != "GIF87a" {
		t.Fatalf("result is not a GIF (header %q)", body)
	}

	// 5. Security probes: traversal + malformed ids must 400/404.
	for _, path := range []string{
		"/api/jobs/../../etc/passwd",
		"/api/jobs/zzzz",
		"/api/jobs/" + created.ID[:16],
	} {
		r, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusBadRequest && r.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 400/404", path, r.StatusCode)
		}
	}
}

// TestCuePNGDimensionCapRejected is the fix for a real finding: isPNG()
// used to check only the 8-byte magic number, never the declared IHDR
// width/height, before handing the file to ffmpeg as a filter-graph input.
// ffmpeg allocates the full raster for a PNG's *declared* dimensions
// regardless of on-disk (compressed) byte size, so a tiny file declaring an
// enormous canvas could force a multi-GB decode-time allocation well under
// MaxCuePNG's byte cap — reachable by any local page via the open-CORS
// multipart POST, no auth needed. This hand-builds just the PNG signature +
// IHDR chunk header (the only 24 bytes validCuePNG reads) declaring a
// 20000x20000 canvas (400M px, MaxPixels is ~8.3M) without ever allocating
// a real 1.6GB image buffer in the test itself.
func TestCuePNGDimensionCapRejected(t *testing.T) {
	tools := testTools(t)
	dir := t.TempDir()

	jm := NewJobManager(filepath.Join(dir, "tmp"), tools)
	srv := NewServer(tools, jm, 512, DefaultWarnMB)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	bomb := make([]byte, 24)
	copy(bomb[0:8], "\x89PNG\r\n\x1a\n")
	copy(bomb[12:16], "IHDR")
	binary.BigEndian.PutUint32(bomb[16:20], 20000) // declared width
	binary.BigEndian.PutUint32(bomb[20:24], 20000) // declared height

	video := makeSampleVideo(t, tools, dir)
	var jb bytes.Buffer
	jw := multipart.NewWriter(&jb)
	settings := map[string]any{
		"trimStart": 0.0, "trimEnd": 1.0, "width": 240, "fps": 10,
		"style": "classic", "encoder": "ffmpeg", "dither": "bayer",
		"cues": []map[string]float64{{"start": 0, "end": 1}},
	}
	sj, _ := json.Marshal(settings)
	_ = jw.WriteField("settings", string(sj))
	vw, _ := jw.CreateFormFile("video", "v.mp4")
	vb, _ := os.ReadFile(video)
	_, _ = vw.Write(vb)
	cw, _ := jw.CreateFormFile("cue_0", "bomb.png")
	_, _ = cw.Write(bomb)
	jw.Close()

	resp, err := http.Post(ts.URL+"/api/jobs", jw.FormDataContentType(), &jb)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("oversized-declared-dimension cue PNG: got HTTP %d, want 400 (body: %s)", resp.StatusCode, body)
	}
}

func TestEstimateBytesSanity(t *testing.T) {
	// 480x270 @15fps for 5s ≈ 2.5 MB ballpark from practitioner figures.
	got := EstimateBytes(480, 270, 15, 5, "ffmpeg", "sierra2_4a")
	if got < 1<<20 || got > 8<<20 {
		t.Errorf("estimate %d bytes outside plausible 1–8 MB band", got)
	}
	if EstimateBytes(480, 270, 15, 5, "ffmpeg", "none") >= got {
		t.Errorf("dither=none should estimate smaller than error diffusion")
	}
}
