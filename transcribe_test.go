package main

// Tests for the local transcription module (v1.4). whisper-cli and the
// sherpa-onnx diarizer are stubbed with tiny shell scripts that emit canned
// output and log every invocation — no models needed, fully deterministic,
// and the one-run-per-clip contract is asserted by counting stub calls.
// The audio-extraction and thumbnail paths use the real ffmpeg (skipped
// when absent). An optional integration test at the bottom runs the REAL
// diarizer when bin/ has it (as scripts/setup-tools.sh --transcribe leaves
// it) and skips otherwise.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubASR installs stub whisper-cli + diarizer scripts into a temp dir,
// points tools at them, and returns the invocation-log path. The stubs:
//
//   - whisper-cli: logs "whisper <args>", writes canned one-word-per-segment
//     JSON ("Hello there. General Kenobi.") to the -of prefix.
//   - diarizer: logs "diar <args>", prints two speaker segments framing the
//     canned words as speaker_00 then speaker_01.
func stubASR(t *testing.T, tools *Tools) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")

	whisper := `#!/bin/sh
echo "whisper $*" >> "$GIFSMITH_STUB_LOG"
prefix=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-of" ]; then prefix="$a"; fi
  prev="$a"
done
[ -n "$prefix" ] || exit 1
cat > "$prefix.json" <<'EOF'
{"transcription":[
  {"text":" Hello","offsets":{"from":500,"to":900}},
  {"text":" there.","offsets":{"from":950,"to":1800}},
  {"text":" General","offsets":{"from":2000,"to":2600}},
  {"text":" Kenobi.","offsets":{"from":2700,"to":3600}},
  {"text":"  ","offsets":{"from":3600,"to":3700}},
  {"text":" bogus","offsets":{"from":4000,"to":4000}}
]}
EOF
`
	diar := `#!/bin/sh
echo "diar $*" >> "$GIFSMITH_STUB_LOG"
echo "OfflineSpeakerDiarizationConfig(...)"
echo "Started"
echo "0.400 -- 1.900 speaker_00"
echo "1.950 -- 3.700 speaker_01"
echo "not a segment line"
`
	for name, body := range map[string]string{
		"whisper-cli": whisper,
		"sherpa-onnx-offline-speaker-diarization": diar,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Model paths need only exist for HasTranscribe-style presence checks.
	for _, name := range []string{"model.bin", "seg.onnx", "emb.onnx"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tools.Whisper = filepath.Join(dir, "whisper-cli")
	tools.Diarizer = filepath.Join(dir, "sherpa-onnx-offline-speaker-diarization")
	tools.WhisperModel = filepath.Join(dir, "model.bin")
	tools.DiarSegModel = filepath.Join(dir, "seg.onnx")
	tools.DiarEmbModel = filepath.Join(dir, "emb.onnx")
	t.Setenv("GIFSMITH_STUB_LOG", logPath)
	return logPath
}

func stubCalls(t *testing.T, logPath, prefix string) int {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix+" ") {
			n++
		}
	}
	return n
}

// ---------- pure-function units ----------

func TestSpeakerLetter(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{0, "A"}, {1, "B"}, {25, "Z"}, {26, "AA"}, {27, "AB"}, {-3, "A"}} {
		if got := speakerLetter(tc.n); got != tc.want {
			t.Errorf("speakerLetter(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestDiarLineRe(t *testing.T) {
	m := diarLineRe.FindStringSubmatch("0.318 -- 6.865 speaker_00")
	if m == nil || m[1] != "0.318" || m[2] != "6.865" || m[3] != "00" {
		t.Fatalf("canonical line failed to parse: %v", m)
	}
	for _, bad := range []string{
		"Started", "Elapsed seconds: 0.037", "OfflineSpeakerDiarizationConfig(x)",
		"a -- b speaker_00", "1.0 -- speaker_01", "",
	} {
		if diarLineRe.MatchString(bad) {
			t.Errorf("%q should not parse as a segment", bad)
		}
	}
}

func TestAssignSpeakers(t *testing.T) {
	words := []WordStamp{
		{Text: "one", Start: 0.5, End: 0.9},   // mid 0.7 → inside seg A
		{Text: "two", Start: 2.0, End: 2.4},   // mid 2.2 → inside seg B
		{Text: "gap", Start: 10.0, End: 10.4}, // outside all → nearest (B)
	}
	segs := []SpeakerSegment{
		{Start: 0.4, End: 1.9, Speaker: 0},
		{Start: 1.95, End: 3.7, Speaker: 1},
	}
	got := assignSpeakers(words, segs)
	if got[0].Speaker != "A" || got[1].Speaker != "B" || got[2].Speaker != "B" {
		t.Fatalf("speakers = %s %s %s, want A B B", got[0].Speaker, got[1].Speaker, got[2].Speaker)
	}
	// No segments at all: degrade to single-speaker, never fail.
	got = assignSpeakers(words, nil)
	for i := range got {
		if got[i].Speaker != "A" {
			t.Fatalf("no-segs fallback: word %d speaker = %q, want A", i, got[i].Speaker)
		}
	}
}

func TestChunksFromWordsGapAndSpeakerSplits(t *testing.T) {
	mk := func(text string, start, end float64, spk string) spokenWord {
		return spokenWord{WordStamp{Text: text, Start: start, End: end}, spk}
	}
	// Gap split: 0.9s silence between "One" and "two".
	chunks := chunksFromWords([]spokenWord{
		mk("One", 0, 0.3, "A"), mk("two", 1.2, 1.5, "A"), mk("three", 1.6, 1.9, "A"),
	})
	if len(chunks) != 2 || chunks[0].Text != "One" || chunks[1].Text != "two three" {
		t.Fatalf("gap split: %+v", chunks)
	}
	if chunks[1].WordCount != 2 || chunks[1].End != 1.9 {
		t.Errorf("chunk meta: %+v", chunks[1])
	}
	// Speaker split.
	chunks = chunksFromWords([]spokenWord{
		mk("Hi", 0, 0.3, "A"), mk("yo", 0.35, 0.6, "B"),
	})
	if len(chunks) != 2 || chunks[0].Speaker != "A" || chunks[1].Speaker != "B" {
		t.Fatalf("speaker split: %+v", chunks)
	}
	// Max-words split at 14.
	var many []spokenWord
	for i := 0; i < 30; i++ {
		s := float64(i) * 0.2
		many = append(many, mk(fmt.Sprintf("w%d", i), s, s+0.15, "A"))
	}
	chunks = chunksFromWords(many)
	if len(chunks) != 3 || chunks[0].WordCount != 14 || chunks[2].WordCount != 2 {
		t.Fatalf("max-words split: %d chunks %+v", len(chunks), chunks)
	}
}

// ---------- stubbed subprocess units ----------

func TestRunWhisperParsesStubJSON(t *testing.T) {
	tools := &Tools{}
	stubASR(t, tools)
	dir := t.TempDir()
	wav := filepath.Join(dir, "a.wav")
	if err := os.WriteFile(wav, []byte("RIFFfake"), 0o644); err != nil {
		t.Fatal(err)
	}
	words, err := runWhisper(t.Context(), tools, wav)
	if err != nil {
		t.Fatal(err)
	}
	// 6 canned segments; the whitespace-only and zero-width ones are dropped.
	if len(words) != 4 || words[0].Text != "Hello" || words[3].Text != "Kenobi." {
		t.Fatalf("words = %+v", words)
	}
	if words[0].Start != 0.5 || words[3].End != 3.6 {
		t.Errorf("timestamps: %+v", words)
	}
}

func TestRunWhisperLanguageFlag(t *testing.T) {
	tools := &Tools{}
	logPath := stubASR(t, tools)
	dir := t.TempDir()
	wav := filepath.Join(dir, "a.wav")
	_ = os.WriteFile(wav, []byte("RIFFfake"), 0o644)

	// Multilingual model name → -l auto.
	if _, err := runWhisper(t.Context(), tools, wav); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "-l auto") {
		t.Errorf("multilingual model should pass -l auto:\n%s", data)
	}
	// English-only model name → -l en.
	enModel := filepath.Join(filepath.Dir(tools.WhisperModel), "ggml-base.en.bin")
	_ = os.WriteFile(enModel, []byte("x"), 0o644)
	tools.WhisperModel = enModel
	if _, err := runWhisper(t.Context(), tools, wav); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(logPath)
	if !strings.Contains(string(data), "-l en") {
		t.Errorf(".en model should pass -l en:\n%s", data)
	}
}

func TestRunDiarizationParsesStubOutput(t *testing.T) {
	tools := &Tools{}
	stubASR(t, tools)
	dir := t.TempDir()
	wav := filepath.Join(dir, "a.wav")
	_ = os.WriteFile(wav, []byte("RIFFfake"), 0o644)
	segs, err := runDiarization(t.Context(), tools, wav, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 || segs[0].Speaker != 0 || segs[1].Speaker != 1 {
		t.Fatalf("segs = %+v", segs)
	}
	if segs[0].Start != 0.4 || segs[1].End != 3.7 {
		t.Errorf("bounds: %+v", segs)
	}
}

// ---------- HTTP end-to-end (real ffmpeg, stubbed ASR) ----------

func newTranscribeTestServer(t *testing.T, tools *Tools, dir string) *httptest.Server {
	t.Helper()
	jm := NewJobManager(filepath.Join(dir, "tmp"), tools)
	tm := NewTranscribeManager(filepath.Join(dir, "tmp"), tools)
	t.Cleanup(tm.Shutdown)
	srv := NewServer(tools, jm, 512, DefaultWarnMB).WithTranscribe(tm)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

func mkAudioClip(t *testing.T, tools *Tools, dir, name string, freq, seconds int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	cmd := exec.Command(tools.FFmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=320x180:rate=30:duration=%d", seconds),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:duration=%d", freq, seconds),
		"-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", "-y", p)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sample clip %s: %v\n%s", name, err, out)
	}
	return p
}

func pollTranscribeDone(t *testing.T, ts *httptest.Server, id string) *TJob {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	final := &TJob{}
	for {
		if time.Now().After(deadline) {
			t.Fatal("transcription did not finish")
		}
		r, err := http.Get(ts.URL + "/api/transcribe/" + id)
		if err != nil {
			t.Fatal(err)
		}
		_ = json.NewDecoder(r.Body).Decode(final)
		r.Body.Close()
		if final.State == TDone {
			return final
		}
		if final.State == TError {
			t.Fatalf("transcribe failed: %s", final.Error)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stubASRNoDiarSegments is stubASR's diarizer variant with zero speaker
// segments (config/status lines only, no "start -- end speaker_N" lines) —
// exercises assignSpeakers' zero-segments fallback for
// TestTranscribeUnattributedSpeaker.
func stubASRNoDiarSegments(t *testing.T, tools *Tools) string {
	t.Helper()
	logPath := stubASR(t, tools)
	diar := "#!/bin/sh\n" +
		`echo "diar $*" >> "$GIFSMITH_STUB_LOG"` + "\n" +
		`echo "OfflineSpeakerDiarizationConfig(...)"` + "\n" +
		`echo "Started"` + "\n"
	if err := os.WriteFile(tools.Diarizer, []byte(diar), 0o755); err != nil {
		t.Fatal(err)
	}
	return logPath
}

// TestTranscribeUnattributedSpeaker: real speech (whisper produces words)
// but diarization returns zero segments must be tagged Source:"unattributed"
// with the real transcribed text intact — not a silent "Speaker A" default
// indistinguishable from genuine diarization (review finding).
func TestTranscribeUnattributedSpeaker(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := testTools(t)
	stubASRNoDiarSegments(t, tools)
	dir := t.TempDir()
	video := mkAudioClip(t, tools, dir, "sample.mp4", 440, 4)
	ts := newTranscribeTestServer(t, tools, dir)

	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	fw, _ := mw.CreateFormFile("video", "v.mp4")
	vb, _ := os.ReadFile(video)
	_, _ = fw.Write(vb)
	mw.Close()
	resp, err := http.Post(ts.URL+"/api/transcribe", mw.FormDataContentType(), &b)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: HTTP %d %s", resp.StatusCode, body)
	}
	var created struct{ ID string }
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	final := pollTranscribeDone(t, ts, created.ID)
	if final.State != TDone {
		t.Fatalf("state = %q, error = %q, want done", final.State, final.Error)
	}
	if len(final.Chunks) == 0 {
		t.Fatal("no chunks produced")
	}
	for _, c := range final.Chunks {
		if c.Source != "unattributed" {
			t.Errorf("chunk source = %q, want %q: %+v", c.Source, "unattributed", c)
		}
		if c.Text == "" {
			t.Errorf("unattributed chunk should still carry real transcribed text: %+v", c)
		}
		if c.Speaker != "A" {
			t.Errorf("chunk speaker = %q, want the zero-segments default %q: %+v", c.Speaker, "A", c)
		}
	}
}

// TestTranscribeEndToEnd drives the HTTP API with the real ffmpeg (audio
// extraction + thumbnail) and the stubbed local ASR toolchain.
func TestTranscribeEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := testTools(t)
	logPath := stubASR(t, tools)
	dir := t.TempDir()
	video := mkAudioClip(t, tools, dir, "sample.mp4", 440, 4)
	ts := newTranscribeTestServer(t, tools, dir)

	// Submit.
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("settings", `{"speakersExpected":2}`)
	fw, _ := mw.CreateFormFile("video", "v.mp4")
	vb, _ := os.ReadFile(video)
	_, _ = fw.Write(vb)
	mw.Close()
	resp, err := http.Post(ts.URL+"/api/transcribe", mw.FormDataContentType(), &b)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: HTTP %d %s", resp.StatusCode, body)
	}
	var created struct{ ID string }
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	final := pollTranscribeDone(t, ts, created.ID)
	// Stub words + stub segments → 2 chunks split on the speaker change.
	if len(final.Chunks) != 2 || final.Chunks[0].Text != "Hello there." || final.Chunks[0].WordCount != 2 {
		t.Fatalf("chunks = %+v", final.Chunks)
	}
	if final.Chunks[0].Speaker != "A" || final.Chunks[1].Speaker != "B" {
		t.Errorf("speakers = %q,%q want A,B", final.Chunks[0].Speaker, final.Chunks[1].Speaker)
	}
	if final.Chunks[0].Start != 0.5 || final.Chunks[1].End != 3.6 {
		t.Errorf("chunk times: %+v", final.Chunks)
	}

	// Thumbnail at chunk midpoint.
	r, err := http.Get(fmt.Sprintf("%s/api/transcribe/%s/thumb?t=%.2f", ts.URL, created.ID, 1.15))
	if err != nil {
		t.Fatal(err)
	}
	img, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || len(img) < 500 || img[0] != 0xFF || img[1] != 0xD8 {
		t.Fatalf("thumbnail: HTTP %d, %d bytes", r.StatusCode, len(img))
	}

	// Hostile probes.
	for _, u := range []string{
		"/api/transcribe/../../etc/passwd",
		"/api/transcribe/zzzz",
		"/api/transcribe/" + created.ID + "/thumb?t=NaN",
		"/api/transcribe/" + created.ID + "/thumb?t=",
		"/api/transcribe/" + created.ID + "/thumb?t=1&clip=99",
		"/api/transcribe/" + created.ID + "/thumb?t=1&clip=-1",
	} {
		r, err := http.Get(ts.URL + u)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode < 400 {
			t.Errorf("GET %s = %d, want error", u, r.StatusCode)
		}
	}

	// Single clip = exactly one whisper run + one diarization run.
	if w, d := stubCalls(t, logPath, "whisper"), stubCalls(t, logPath, "diar"); w != 1 || d != 1 {
		t.Errorf("single clip made %d whisper / %d diar runs, want 1/1", w, d)
	}
	// The speakers hint must reach the diarizer as a cluster count.
	if data, _ := os.ReadFile(logPath); !strings.Contains(string(data), "--clustering.num-clusters=2") {
		t.Errorf("speakersExpected=2 not passed to diarizer:\n%s", data)
	}
}

// TestTranscribeNoAudioFallback: a clip with no audio track must not fail
// the job or hang — it falls back to scene-cut detection (scenedetect.go)
// and produces "scene"-sourced chunks instead of dialogue chunks, without
// ever invoking whisper or the diarizer.
func TestTranscribeNoAudioFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := testTools(t)
	logPath := stubASR(t, tools)
	dir := t.TempDir()
	video := makeMultiSceneVideo(t, tools, dir) // video-only fixture: no -i sine, no -c:a
	ts := newTranscribeTestServer(t, tools, dir)

	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	fw, _ := mw.CreateFormFile("video", "v.mp4")
	vb, _ := os.ReadFile(video)
	_, _ = fw.Write(vb)
	mw.Close()
	resp, err := http.Post(ts.URL+"/api/transcribe", mw.FormDataContentType(), &b)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: HTTP %d %s", resp.StatusCode, body)
	}
	var created struct{ ID string }
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	final := pollTranscribeDone(t, ts, created.ID)
	if final.State != TDone {
		t.Fatalf("state = %q, error = %q, want done", final.State, final.Error)
	}
	if len(final.Chunks) == 0 {
		t.Fatal("no chunks produced for a no-audio clip; want a scene-cut fallback")
	}
	for _, c := range final.Chunks {
		if c.Source != "scene" {
			t.Errorf("chunk source = %q, want %q: %+v", c.Source, "scene", c)
		}
		if c.Text != "" || c.WordCount != 0 || c.Speaker != "" {
			t.Errorf("scene chunk carries dialogue fields it shouldn't: %+v", c)
		}
	}
	// The whole point: no audio means no whisper/diarizer invocation at all.
	if w, d := stubCalls(t, logPath, "whisper"), stubCalls(t, logPath, "diar"); w != 0 || d != 0 {
		t.Errorf("no-audio clip ran %d whisper / %d diar calls, want 0/0", w, d)
	}
}

// TestTranscribeMixedAudioFallback: in a multi-clip job, a silent clip
// alongside a normal one must not fail the whole job — the silent clip
// gets scene-sourced chunks while its sibling still gets real dialogue
// chunks, preserving the existing per-clip independence design.
func TestTranscribeMixedAudioFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := testTools(t)
	stubASR(t, tools)
	dir := t.TempDir()
	silent := makeMultiSceneVideo(t, tools, dir)
	spoken := mkAudioClip(t, tools, dir, "spoken.mp4", 440, 4)
	ts := newTranscribeTestServer(t, tools, dir)

	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for i, p := range []string{silent, spoken} {
		fw, _ := mw.CreateFormFile(fmt.Sprintf("video_%d", i), "v.mp4")
		vb, _ := os.ReadFile(p)
		_, _ = fw.Write(vb)
	}
	mw.Close()
	resp, err := http.Post(ts.URL+"/api/transcribe", mw.FormDataContentType(), &b)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: HTTP %d %s", resp.StatusCode, body)
	}
	var created struct{ ID string }
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	final := pollTranscribeDone(t, ts, created.ID)
	if final.State != TDone {
		t.Fatalf("state = %q, error = %q, want done", final.State, final.Error)
	}
	var sceneClips, speechClips int
	for _, c := range final.Chunks {
		switch {
		case c.Source == "scene" && c.Clip == 0:
			sceneClips++
		case c.Source == "" && c.Clip == 1:
			speechClips++
		default:
			t.Errorf("unexpected chunk: %+v", c)
		}
	}
	if sceneClips == 0 {
		t.Error("silent clip 0 produced no scene chunks")
	}
	if speechClips == 0 {
		t.Error("spoken clip 1 produced no dialogue chunks")
	}
}

// TestTranscribeMultiClip: N clips must produce N whisper runs and N
// diarization runs (the design constraint that keeps speaker labels
// clip-scoped), clip-tagged chunks with clip-LOCAL times, and
// clip-addressable thumbnails.
func TestTranscribeMultiClip(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := testTools(t)
	logPath := stubASR(t, tools)
	dir := t.TempDir()
	v0 := mkAudioClip(t, tools, dir, "c0.mp4", 440, 3)
	v1 := mkAudioClip(t, tools, dir, "c1.mp4", 880, 3)
	ts := newTranscribeTestServer(t, tools, dir)

	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("settings", `{"speakersExpected":2}`)
	for i, p := range []string{v0, v1} {
		fw, _ := mw.CreateFormFile(fmt.Sprintf("video_%d", i), "v.mp4")
		vb, _ := os.ReadFile(p)
		_, _ = fw.Write(vb)
	}
	mw.Close()
	resp, err := http.Post(ts.URL+"/api/transcribe", mw.FormDataContentType(), &b)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: HTTP %d %s", resp.StatusCode, body)
	}
	var created struct {
		ID        string `json:"id"`
		ClipCount int    `json:"clipCount"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.ClipCount != 2 {
		t.Fatalf("clipCount = %d, want 2", created.ClipCount)
	}

	final := pollTranscribeDone(t, ts, created.ID)

	// The core constraint: 2 clips ⇒ exactly 2 whisper + 2 diarization
	// runs. One concatenated run would be 1/1 — and would bleed speaker
	// labels across clips.
	if w, d := stubCalls(t, logPath, "whisper"), stubCalls(t, logPath, "diar"); w != 2 || d != 2 {
		t.Fatalf("2 clips made %d whisper / %d diar runs, want 2/2", w, d)
	}
	if final.ClipsDone != 2 {
		t.Errorf("clipsDone = %d, want 2", final.ClipsDone)
	}

	// Both clips' chunks present, clip-tagged, with clip-LOCAL times (the
	// stub returns the same words for both, so times must match).
	if len(final.Chunks) != 4 {
		t.Fatalf("chunks = %d, want 4 (2 per clip)", len(final.Chunks))
	}
	for i, c := range final.Chunks {
		wantClip := 0
		if i >= 2 {
			wantClip = 1
		}
		if c.Clip != wantClip {
			t.Errorf("chunk %d clip = %d, want %d", i, c.Clip, wantClip)
		}
	}
	if final.Chunks[2].Start != 0.5 || final.Chunks[2].Speaker != "A" {
		t.Errorf("clip 1 chunk times must be clip-local: %+v", final.Chunks[2])
	}

	// Thumbnails address clips independently.
	for clip := 0; clip < 2; clip++ {
		r, err := http.Get(fmt.Sprintf("%s/api/transcribe/%s/thumb?clip=%d&t=1.0", ts.URL, created.ID, clip))
		if err != nil {
			t.Fatal(err)
		}
		img, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != 200 || len(img) < 500 || img[0] != 0xFF || img[1] != 0xD8 {
			t.Fatalf("thumb clip=%d: HTTP %d, %d bytes", clip, r.StatusCode, len(img))
		}
	}
	// Out-of-range clip index on a real job: 4xx, not a crash.
	r, err := http.Get(ts.URL + "/api/transcribe/" + created.ID + "/thumb?clip=2&t=1.0")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode < 400 {
		t.Errorf("thumb clip=2 on 2-clip job = %d, want 4xx", r.StatusCode)
	}
}

// TestTranscribeUnavailable: a server whose toolchain lacks the ASR pieces
// never registers a TranscribeManager; the endpoints must 503, not crash.
func TestTranscribeUnavailable(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := testTools(t)
	dir := t.TempDir()
	jm := NewJobManager(filepath.Join(dir, "tmp"), tools)
	srv := NewServer(tools, jm, 512, DefaultWarnMB) // no WithTranscribe
	ts := httptest.NewServer(srv)
	defer ts.Close()

	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("settings", `{}`)
	fw, _ := mw.CreateFormFile("video", "v.mp4")
	_, _ = fw.Write([]byte("not-a-video"))
	mw.Close()
	resp, err := http.Post(ts.URL+"/api/transcribe", mw.FormDataContentType(), &b)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

// TestTranscribeRateLimit: the hourly CPU-burn budget (restored per review
// round 4) is charged per clip at Submit and rejects whole jobs; NewJobDir
// refuses new reservations once the window is spent.
func TestTranscribeRateLimit(t *testing.T) {
	tools := &Tools{FFmpeg: "/nonexistent/ffmpeg", FFprobe: "/nonexistent/ffprobe"}
	stubASR(t, tools)
	tm := NewTranscribeManager(t.TempDir(), tools)
	defer tm.Shutdown()

	oneClip := []TClip{{Path: "/nonexistent/clip", Duration: 1}}
	for i := 0; i < TSubmitsPerHour; i++ {
		id, dir, err := tm.NewJobDir()
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		if _, err := tm.Submit(id, dir, oneClip, 0); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	// Budget spent: the next reservation is refused outright.
	if _, _, err := tm.NewJobDir(); err != ErrRateLimited {
		t.Fatalf("31st reservation: err = %v, want ErrRateLimited", err)
	}

	// Whole-job atomicity: with budget for < N clips, an N-clip Submit is
	// rejected whole (fresh manager: spend all but one unit, then try 2).
	tm2 := NewTranscribeManager(t.TempDir(), tools)
	defer tm2.Shutdown()
	for i := 0; i < TSubmitsPerHour-1; i++ {
		id, dir, err := tm2.NewJobDir()
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		if _, err := tm2.Submit(id, dir, oneClip, 0); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	id, dir, err := tm2.NewJobDir()
	if err != nil {
		t.Fatal(err)
	}
	twoClips := []TClip{{Path: "/nonexistent/a", Duration: 1}, {Path: "/nonexistent/b", Duration: 1}}
	if _, err := tm2.Submit(id, dir, twoClips, 0); err != ErrRateLimited {
		t.Fatalf("2-clip submit on 1-unit budget: err = %v, want ErrRateLimited", err)
	}
	tm2.Release(id)
}

// TestRealDiarizationIntegration exercises the REAL sherpa-onnx binary and
// models when present in bin/ (scripts/setup-tools.sh --transcribe) using
// the 4-speaker sample from k2-fsa's release assets if available. Skips
// cleanly otherwise, so `go test` needs no 40 MB model checkout.
func TestRealDiarizationIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools, err := findTools("bin", "")
	if err != nil || tools.Diarizer == "" || tools.DiarSegModel == "" || tools.DiarEmbModel == "" {
		t.Skip("real diarization toolchain not installed in bin/")
	}
	wav := filepath.Join("testdata", "four-speakers.wav")
	if _, err := os.Stat(wav); err != nil {
		t.Skip("testdata/four-speakers.wav not present (fetch from k2-fsa speaker-segmentation-models release assets)")
	}
	segs, err := runDiarization(t.Context(), tools, wav, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) < 4 {
		t.Fatalf("expected several segments, got %d", len(segs))
	}
	speakers := map[int]bool{}
	for i, s := range segs {
		if s.End <= s.Start {
			t.Errorf("segment %d not increasing: %+v", i, s)
		}
		if i > 0 && s.Start < segs[i-1].Start {
			t.Errorf("segments not sorted at %d", i)
		}
		speakers[s.Speaker] = true
	}
	if len(speakers) != 4 {
		t.Errorf("expected 4 distinct speakers, got %d", len(speakers))
	}
	// Reproducibility on the same machine+model: a second run must match.
	segs2, err := runDiarization(t.Context(), tools, wav, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs2) != len(segs) {
		t.Fatalf("second run: %d segments vs %d", len(segs2), len(segs))
	}
	for i := range segs {
		if segs[i] != segs2[i] {
			t.Errorf("segment %d differs across runs: %+v vs %+v", i, segs[i], segs2[i])
		}
	}
}
