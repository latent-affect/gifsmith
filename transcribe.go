package main

// transcribe.go — the Transcribe feature: extract audio from each uploaded
// clip, transcribe it with whisper.cpp and diarize it with sherpa-onnx —
// entirely on this machine — and serve the result as timestamped dialogue
// chunks plus per-chunk scene thumbnails.
//
// Privacy (also in README/CVE-AUDIT): as of v1.4 NOTHING leaves the machine,
// ever. v1.1–v1.3 sent extracted audio to AssemblyAI's API; that whole
// egress path (and its API-key plumbing) is gone. The server still binds
// loopback-only; the transcription work is two pinned local subprocesses.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// WordStamp is one word with second-based timestamps.
type WordStamp struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// TranscriptChunk is one dialogue chunk (normally one speaker utterance).
//
// Multi-clip (v1.3): Clip is the 0-based index of the source clip this
// chunk came from, and Start/End/Words are CLIP-LOCAL source seconds. Each
// clip is a separate transcription+diarization run by design — speaker
// labels are therefore scoped per clip ("Speaker A" in clip 2 is unrelated
// to "Speaker A" in clip 1), which is exactly what keeps a new clip from
// showing up as a confusing "Speaker D" continuation. The UI badges the
// clip number next to the speaker so the scope is visible.
//
// No-audio fallback (v1.5): Source is "" for a normal dialogue chunk
// (omitted from JSON, so existing clients see no change) or "scene" for a
// camera-cut chunk from scenedetect.go's no-audio fallback — Speaker/Text/
// WordCount/Words are all zero-value for those; the timing scaffold is the
// Start/End window itself, not transcribed content.
//
// Undisclosed-diarization fallback (v1.5, review finding): Source is
// "unattributed" when whisper DID transcribe real speech but sherpa-onnx's
// diarization returned zero segments — assignSpeakers (localasr.go) then
// defaults every word to "Speaker A", a plausible-looking label that isn't
// actually diarized. Text/WordCount/Words are all real here, unlike "scene"
// — only the Speaker attribution is a stand-in, which is why this is its
// own Source value rather than being folded into "scene".
type TranscriptChunk struct {
	Clip      int         `json:"clip"`
	Speaker   string      `json:"speaker"`
	Start     float64     `json:"start"`
	End       float64     `json:"end"`
	Text      string      `json:"text"`
	WordCount int         `json:"wordCount"`
	Words     []WordStamp `json:"words"`
	Source    string      `json:"source,omitempty"`
}

type TranscribeState string

const (
	TQueued       TranscribeState = "queued"
	TExtracting   TranscribeState = "extracting"
	TTranscribing TranscribeState = "transcribing"
	TDiarizing    TranscribeState = "diarizing"
	TDone         TranscribeState = "done"
	TError        TranscribeState = "error"
	TCancelled    TranscribeState = "cancelled"

	// Per-clip time budget. Whisper on CPU typically runs well under 1×
	// real-time with the base model; 20 min/clip is a hang guard, not a
	// speed expectation. The job deadline scales with clip count, capped —
	// and starts ticking when PROCESSING starts, not while queued.
	transcribeWait = 20 * time.Minute

	// Resource discipline (adversarial-review findings, rounds 1–4): the
	// transcribe endpoints are reachable by any local web page (documented
	// CORS posture), so every axis is capped. With v1.4 the scarce
	// resource is local CPU rather than API credits, but the hourly budget
	// STAYS (round-4 finding: without it a hostile page could keep the CPU
	// pinned indefinitely by submit→delete→resubmit cycling under the
	// concurrency cap alone) — generous enough to be invisible to a real
	// user, charged one unit per clip at Submit, whole-job rejection.
	//
	// MaxRunningTJobs gates concurrent PROCESSING via a semaphore in run()
	// (excess jobs sit in "queued"); reservations get their own cap
	// (round-4 finding: reservations counted against the running cap let
	// two stalled uploads block the feature outright).
	MaxRunningTJobs  = 2
	MaxReservations  = 8
	MaxTotalTJobs    = 40
	TSubmitsPerHour  = 30
	MaxThumbsPerJob  = 300
	thumbConcurrency = 2
)

// TClip is one source clip inside a transcribe job.
type TClip struct {
	Path     string  // server-side path of the uploaded video
	Duration float64 // probed duration, seconds
}

type TJob struct {
	ID        string            `json:"id"`
	State     TranscribeState   `json:"state"`
	Error     string            `json:"error,omitempty"`
	Chunks    []TranscriptChunk `json:"chunks,omitempty"`
	Duration  float64           `json:"duration"`  // sum of probed clip durations, seconds
	ClipCount int               `json:"clipCount"` // number of clips (= transcription runs)
	ClipsDone int               `json:"clipsDone"` // clips fully transcribed so far
	Created   int64             `json:"created"`

	dir     string
	clips   []TClip
	timeout time.Duration // actual deadline (scales with clip count)
	cancel  context.CancelFunc
	mu      sync.Mutex
}

func (j *TJob) snapshot() TJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	return TJob{
		ID: j.ID, State: j.State, Error: j.Error, Chunks: j.Chunks,
		Duration: j.Duration, ClipCount: j.ClipCount, ClipsDone: j.ClipsDone,
		Created: j.Created,
	}
}

// ErrRateLimited is returned when the per-hour submission budget is spent.
var ErrRateLimited = fmt.Errorf("transcription rate limit reached (%d clips/hour); try again later", TSubmitsPerHour)

type TranscribeManager struct {
	mu       sync.Mutex
	jobs     map[string]*TJob
	reserved map[string]bool // ids handed out by NewJobDir, not yet Submitted
	created  []time.Time     // per-clip submission timestamps in the rate window
	tmpRoot  string
	tools    *Tools
	ttl      time.Duration
	stop     chan struct{}
	runSem   chan struct{} // gates concurrent heavy processing (whisper/diar)
	thumbSem chan struct{}
}

func NewTranscribeManager(tmpRoot string, tools *Tools) *TranscribeManager {
	m := &TranscribeManager{
		jobs:     make(map[string]*TJob),
		reserved: make(map[string]bool),
		tmpRoot:  tmpRoot,
		tools:    tools,
		ttl:      2 * time.Hour,
		stop:     make(chan struct{}),
		runSem:   make(chan struct{}, MaxRunningTJobs),
		thumbSem: make(chan struct{}, thumbConcurrency),
	}
	go m.sweep()
	return m
}

// pruneRateWindowLocked drops rate-window entries older than an hour.
// Callers hold m.mu.
func (m *TranscribeManager) pruneRateWindowLocked(now time.Time) {
	keep := m.created[:0]
	for _, t := range m.created {
		if now.Sub(t) < time.Hour {
			keep = append(keep, t)
		}
	}
	m.created = keep
}

// NewJobDir reserves a slot and creates the job directory. The reservation
// closes the check-then-act gap between the cap check and Submit (TOCTOU
// finding): concurrent uploads each hold a counted reservation — against
// their OWN cap, not the running cap, so a stalled upload cannot starve
// real jobs (round-4 finding). Callers MUST either Submit(id, …) or
// Release(id).
func (m *TranscribeManager) NewJobDir() (string, string, error) {
	m.mu.Lock()
	m.pruneRateWindowLocked(time.Now())
	switch {
	case len(m.created) >= TSubmitsPerHour:
		m.mu.Unlock()
		return "", "", ErrRateLimited
	case len(m.reserved) >= MaxReservations,
		len(m.jobs)+len(m.reserved) >= MaxTotalTJobs:
		m.mu.Unlock()
		return "", "", ErrTooManyJobs
	}
	id := newID()
	m.reserved[id] = true
	// NOTE: the rate-window charge happens in Submit, not here — malformed
	// uploads that never become jobs must not burn the budget.
	m.mu.Unlock()

	dir := filepath.Join(m.tmpRoot, "t-"+id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.Release(id)
		return "", "", err
	}
	return id, dir, nil
}

// Release frees a reservation whose upload failed before Submit.
func (m *TranscribeManager) Release(id string) {
	m.mu.Lock()
	delete(m.reserved, id)
	m.mu.Unlock()
}

// Submit starts the per-clip extract→transcribe→diarize pipeline. Every
// clip is one whisper run plus one diarization run, sequentially — the
// same one-run-per-clip contract the AssemblyAI design had, for the same
// reason: clip-scoped speaker labels.
//
// The hourly CPU budget is charged one unit per clip, atomically — a
// multi-clip job that would blow the budget is rejected whole rather than
// partially transcribed. Returns ErrRateLimited in that case; the caller
// must then Release the reservation.
func (m *TranscribeManager) Submit(id, dir string, clips []TClip, speakersExpected int) (*TJob, error) {
	var total float64
	for _, c := range clips {
		total += c.Duration
	}
	// Deadline scales with clip count (the runs are sequential), bounded.
	// It is applied in run() AFTER the processing slot is acquired, so
	// time spent queued behind other jobs doesn't eat the budget.
	wait := transcribeWait * time.Duration(len(clips))
	if wait > 90*time.Minute {
		wait = 90 * time.Minute
	}

	m.mu.Lock()
	now := time.Now()
	m.pruneRateWindowLocked(now)
	if len(m.created)+len(clips) > TSubmitsPerHour {
		m.mu.Unlock()
		return nil, ErrRateLimited
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &TJob{
		ID: id, State: TQueued, Duration: total,
		ClipCount: len(clips), Created: now.Unix(),
		dir: dir, clips: clips, timeout: wait, cancel: cancel,
	}
	delete(m.reserved, id) // reservation becomes a real job
	m.jobs[id] = j
	for range clips { // budget: one unit per whisper+diarization run
		m.created = append(m.created, now)
	}
	m.mu.Unlock()

	go m.run(ctx, j, speakersExpected)
	return j, nil
}

func (m *TranscribeManager) setState(j *TJob, s TranscribeState) {
	j.mu.Lock()
	j.State = s
	j.mu.Unlock()
}

func (m *TranscribeManager) fail(j *TJob, ctx context.Context, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if ctx.Err() == context.Canceled {
		prior := j.State
		j.State = TCancelled
		Debug.Add("transcribe", "job %s cancelled during %s", j.ID[:8], prior)
		return
	}
	j.State = TError
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// Report the job's ACTUAL deadline — it scales with clip count
		// (review finding LOW-6), so naming transcribeWait would be wrong.
		j.Error = fmt.Sprintf("transcription exceeded the %d-minute limit (%v)", int(j.timeout.Minutes()), err)
	} else {
		j.Error = err.Error()
	}
	Debug.Add("transcribe", "job %s FAILED: %s", j.ID[:8], j.Error)
}

func (m *TranscribeManager) run(parent context.Context, j *TJob, speakersExpected int) {
	defer j.cancel()

	// Wait for a processing slot: at most MaxRunningTJobs jobs do heavy
	// whisper/diarization work at once; the rest report "queued".
	select {
	case m.runSem <- struct{}{}:
		defer func() { <-m.runSem }()
	case <-parent.Done(): // cancelled while queued
		m.fail(j, parent, parent.Err())
		return
	case <-m.stop:
		return
	}
	// The wall-clock budget starts NOW, not at Submit.
	ctx, tcancel := context.WithTimeout(parent, j.timeout)
	defer tcancel()

	Debug.Add("transcribe", "job %s start: clips=%d duration=%.1fs speakersExpected=%d model=%s (local)",
		j.ID[:8], j.ClipCount, j.Duration, speakersExpected, filepath.Base(m.tools.WhisperModel))

	// One full extract→transcribe→diarize cycle PER CLIP, sequentially:
	// each clip is its own whisper+diarization run so speaker labels stay
	// clip-scoped (see the TranscriptChunk contract note). Any clip failing
	// fails the whole job — a silently half-transcribed multi-clip result
	// would be worse than an honest error.
	var all []TranscriptChunk
	for k, clip := range j.clips {
		label := fmt.Sprintf("clip %d/%d", k+1, len(j.clips))

		// 1. Extract mono 16 kHz PCM WAV — the native input format of both
		// whisper.cpp and sherpa-onnx. (v1.1–v1.3 encoded MP3 to shrink the
		// AssemblyAI upload; with no upload there is nothing to shrink, and
		// dropping libmp3lame removes a dependency from the hardened
		// ffmpeg build's enable-list.)
		m.setState(j, TExtracting)
		audioPath := filepath.Join(j.dir, fmt.Sprintf("audio_%02d.wav", k))
		args := []string{"-hide_banner", "-nostdin", "-loglevel", "error",
			"-protocol_whitelist", "file,pipe",
			"-i", clip.Path, "-vn", "-ac", "1", "-ar", "16000",
			"-c:a", "pcm_s16le", "-y", audioPath}
		cmd := exec.CommandContext(ctx, m.tools.FFmpeg, args...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		noAudio := false
		if err := cmd.Run(); err != nil {
			// Some clips have no audio track at all — fall back to camera-cut
			// timestamps (scenedetect.go) instead of failing the whole job.
			if strings.Contains(stderr.String(), "does not contain any stream") ||
				strings.Contains(stderr.String(), "Output file does not contain") {
				noAudio = true
			} else {
				m.fail(j, ctx, fmt.Errorf("%s: audio extraction failed: %s", label, firstLine(stderr.String())))
				return
			}
		}
		if !noAudio {
			if fi, err := os.Stat(audioPath); err != nil || fi.Size() == 0 {
				noAudio = true
			} else {
				Debug.Add("transcribe", "job %s %s audio extracted: %.2f MB wav", j.ID[:8], label, float64(fi.Size())/1048576)
			}
		}

		if noAudio {
			Debug.Add("transcribe", "job %s %s has no audio — falling back to scene-cut detection", j.ID[:8], label)
			chunks, err := sceneChunksForClip(ctx, m.tools, clip)
			if err != nil {
				m.fail(j, ctx, fmt.Errorf("%s has no audio track, and scene detection also failed: %s", label, err))
				return
			}
			for i := range chunks {
				chunks[i].Clip = k
			}
			all = append(all, chunks...)
			j.mu.Lock()
			j.ClipsDone = k + 1
			j.mu.Unlock()
			Debug.Add("transcribe", "job %s %s done via scene detection: %d chunks", j.ID[:8], label, len(chunks))
			continue
		}

		// 2. Transcribe locally (whisper.cpp): word-level timestamps.
		m.setState(j, TTranscribing)
		words, err := runWhisper(ctx, m.tools, audioPath)
		if err != nil {
			m.fail(j, ctx, fmt.Errorf("%s: %w", label, err))
			return
		}

		// 3. Diarize locally (sherpa-onnx): who-spoke-when, clip-scoped.
		m.setState(j, TDiarizing)
		segs, err := runDiarization(ctx, m.tools, audioPath, speakersExpected)
		if err != nil {
			m.fail(j, ctx, fmt.Errorf("%s: %w", label, err))
			return
		}

		chunks := chunksFromWords(assignSpeakers(words, segs))
		for i := range chunks {
			chunks[i].Clip = k
			if len(segs) == 0 {
				// Diarization found zero speaker segments even though whisper
				// DID transcribe real speech — assignSpeakers defaults every
				// word to "Speaker A", a plausible-looking label that isn't
				// actually diarized. Disclosed the same way the no-audio
				// scene-cut fallback is (review finding): a distinct Source
				// tag the frontend badges differently, so this doesn't read
				// as a confident real diarization result.
				chunks[i].Source = "unattributed"
			}
		}
		all = append(all, chunks...)
		j.mu.Lock()
		j.ClipsDone = k + 1
		j.mu.Unlock()
		Debug.Add("transcribe", "job %s %s done: %d chunks (words=%d, speakerSegs=%d)",
			j.ID[:8], label, len(chunks), len(words), len(segs))
		_ = os.Remove(audioPath) // job dir hygiene; thumbs re-read the video, not the wav
	}

	j.mu.Lock()
	j.Chunks = all
	j.State = TDone
	if len(all) == 0 {
		j.State = TError
		j.Error = "transcription returned no speech (silent or non-speech audio?)"
	}
	j.mu.Unlock()
	// Log FAILED, not "done", for the zero-chunk case above — otherwise the
	// exported debug trace shows "done: 0 chunks" for a run the API itself
	// reports as state:"error" (review finding: misleading internal trace,
	// the actual API response was already correct either way).
	if len(all) == 0 {
		Debug.Add("transcribe", "job %s FAILED: no speech across %d clips", j.ID[:8], j.ClipCount)
	} else {
		Debug.Add("transcribe", "job %s done: %d chunks across %d clips", j.ID[:8], len(all), j.ClipCount)
	}
}

func (m *TranscribeManager) Get(id string) (*TJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	return j, ok
}

func (m *TranscribeManager) Cancel(id string) error {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if ok {
		delete(m.jobs, id)
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such job")
	}
	j.cancel()
	// In-flight thumbnail extractions can outlive the 500ms grace and
	// re-create files mid-RemoveAll — retry a few times (review finding).
	go func() {
		for i := 0; i < 4; i++ {
			time.Sleep(500 * time.Millisecond)
			if os.RemoveAll(j.dir) == nil {
				if _, err := os.Stat(j.dir); err != nil {
					return
				}
			}
		}
		Debug.Add("transcribe", "job %s: could not fully remove %s after 4 attempts", id[:8], j.dir)
	}()
	return nil
}

// Thumbnail extracts (and caches) a scene frame at clip-local time t from
// clip index `clip` of a job. Served only for finished jobs; per-job
// count-capped; concurrency-gated; written via tmp+rename so a concurrent
// request can never see a partial JPEG (all adversarial-review findings).
func (m *TranscribeManager) Thumbnail(ctx context.Context, id string, clip int, t float64) (string, error) {
	j, ok := m.Get(id)
	if !ok {
		return "", fmt.Errorf("no such job")
	}
	j.mu.Lock()
	dir := j.dir
	state := j.State
	nclips := len(j.clips)
	var video string
	var dur float64
	if clip >= 0 && clip < nclips {
		video = j.clips[clip].Path
		dur = j.clips[clip].Duration
	}
	j.mu.Unlock()
	if state != TDone {
		return "", fmt.Errorf("thumbnails are available once transcription is done")
	}
	if video == "" {
		return "", fmt.Errorf("clip index %d out of range [0,%d)", clip, nclips)
	}
	if math.IsNaN(t) || t < 0 {
		t = 0
	}
	if dur > 0 && t > dur {
		t = dur
	}
	if t > 86400 { // absolute ceiling: no int64 games via unbounded t
		t = 86400
	}
	// Cache keyed by clip + centisecond.
	name := fmt.Sprintf("thumb_%02d_%08d.jpg", clip, int64(math.Round(t*100)))
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}

	select {
	case m.thumbSem <- struct{}{}:
		defer func() { <-m.thumbSem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// Per-job cap, checked under the semaphore so concurrent requests can't
	// slip past it: a chunk list needs one thumb per chunk; hundreds is
	// plenty, and an uncapped endpoint is a disk-filling primitive.
	// Scaled by clip count (review finding LOW-7): 8 dialogue-heavy clips
	// legitimately exceed a flat 300, and the cap still spans ALL clips'
	// thumbs (the glob is clip-agnostic), so ?clip= cannot bypass it.
	limit := MaxThumbsPerJob * nclips
	if existing, _ := filepath.Glob(filepath.Join(dir, "thumb_*.jpg")); len(existing) >= limit {
		return "", fmt.Errorf("thumbnail limit reached for this job (%d)", limit)
	}

	tmp := p + ".tmp"
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error",
		"-protocol_whitelist", "file,pipe",
		"-ss", ffFloat(t), "-i", video,
		"-frames:v", "1", "-vf", "scale=320:-2", "-q:v", "5", "-f", "image2", "-update", "1", "-y", tmp}
	cmd := exec.CommandContext(ctx, m.tools.FFmpeg, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("thumbnail extraction failed: %s", firstLine(stderr.String()))
	}
	if fi, err := os.Stat(tmp); err != nil || fi.Size() == 0 {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("no frame at %.2fs", t)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return p, nil
}

func (m *TranscribeManager) sweep() {
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-tick.C:
		}
		cutoff := time.Now().Add(-m.ttl).Unix()
		var doomed []string
		m.mu.Lock()
		for id, j := range m.jobs {
			j.mu.Lock()
			expired := j.Created < cutoff
			running := j.State == TQueued || j.State == TExtracting ||
				j.State == TTranscribing || j.State == TDiarizing
			j.mu.Unlock()
			if expired && !running {
				delete(m.jobs, id)
				doomed = append(doomed, j.dir)
			}
		}
		m.mu.Unlock()
		for _, d := range doomed {
			if err := os.RemoveAll(d); err != nil {
				Debug.Add("transcribe", "TTL sweep: failed to remove %s: %v", d, err)
			}
		}
	}
}

func (m *TranscribeManager) Shutdown() {
	close(m.stop)
	m.mu.Lock()
	for _, j := range m.jobs {
		j.cancel()
	}
	m.mu.Unlock()
	time.Sleep(300 * time.Millisecond)
}
