package main

// server.go — HTTP API. Loopback-only by design; CORS is open ("*") because
// the frontend may be opened as a local file (file:// pages send
// "Origin: null"; we send "*", never the literal "null" — see
// docs/domain-briefing.md §CORS).
//
// Honest threat framing (from the adversarial review): with ACAO "*", a web
// page the user merely visits CAN drive this API (multipart POSTs are
// CORS-safelisted) and read the responses. There are no cookies, no
// credentials, and job IDs are 128-bit crypto-random, so no user data is
// exposed — the residual risk is resource abuse, which is bounded by the
// pixel cap, MaxLiveJobs, the 1-encode semaphore and upload limits. A Host
// allowlist below blocks DNS-rebinding, which would otherwise defeat any
// origin-based scheme. Requiring auth tokens would break the file:// use
// case this tool exists for; the trade-off is documented in CVE-AUDIT.md.

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const AppVersion = "1.4.0"

var idRe = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Server struct {
	tools       *Tools
	jobs        *JobManager
	tjobs       *TranscribeManager // nil-safe: endpoints 503 when absent
	maxUploadMB int64
	warnMB      float64
	mux         *http.ServeMux
	fontsDir    string // overridable via WithFontsDir; see main.go's defaultFontsDir
}

func NewServer(tools *Tools, jobs *JobManager, maxUploadMB int64, warnMB float64) *Server {
	s := &Server{
		tools:       tools,
		jobs:        jobs,
		maxUploadMB: maxUploadMB,
		warnMB:      warnMB,
		mux:         http.NewServeMux(),
		fontsDir:    "fonts", // CWD-relative default; main.go overrides to an executable-relative path
	}
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /fonts/{name}", s.handleFont)
	s.mux.HandleFunc("GET /api/config", s.handleConfig)
	s.mux.HandleFunc("POST /api/jobs", s.handleCreateJob)
	s.mux.HandleFunc("GET /api/jobs/{id}", s.handleJobStatus)
	s.mux.HandleFunc("DELETE /api/jobs/{id}", s.handleJobCancel)
	s.mux.HandleFunc("GET /api/jobs/{id}/result", s.handleJobResult)
	s.mux.HandleFunc("POST /api/jobs/{id}/reveal", s.handleJobReveal)
	s.mux.HandleFunc("GET /api/debug/log", s.handleDebugLog)
	s.mux.HandleFunc("POST /api/transcribe", s.handleTranscribeCreate)
	s.mux.HandleFunc("GET /api/transcribe/{id}", s.handleTranscribeStatus)
	s.mux.HandleFunc("DELETE /api/transcribe/{id}", s.handleTranscribeCancel)
	s.mux.HandleFunc("GET /api/transcribe/{id}/thumb", s.handleTranscribeThumb)
	return s
}

// WithTranscribe attaches the transcription manager.
func (s *Server) WithTranscribe(tm *TranscribeManager) *Server {
	s.tjobs = tm
	return s
}

// WithFontsDir overrides where handleFont looks for scripts/fetch-fonts.sh's
// output. Defaults to "fonts" (CWD-relative) if never called — main.go
// overrides this to an executable-relative path (cross-unit-contract
// review finding: bin/ was already resolved relative to os.Executable()'s
// directory via defaultToolsDir, robust to whatever CWD the process is
// launched from, but fonts/ was resolving against the CWD instead — the two
// lookups now share the same convention).
func (s *Server) WithFontsDir(dir string) *Server {
	s.fontsDir = dir
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// DNS-rebinding guard: a hostile domain resolving to 127.0.0.1 would
	// arrive with its own Host header; only loopback names are served.
	if !loopbackHost(r.Host) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	// CORS for the file:// frontend. "*" (never "null"): non-credentialed.
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Request trace for the debug log (skip the polling endpoints' happy
	// path noise: they're logged only on non-200s via statusRecorder).
	rec := &statusRecorder{ResponseWriter: w, status: 200}
	start := time.Now()
	s.mux.ServeHTTP(rec, r)
	poll := r.Method == http.MethodGet &&
		(strings.HasPrefix(r.URL.Path, "/api/jobs/") || strings.HasPrefix(r.URL.Path, "/api/transcribe/") ||
			r.URL.Path == "/api/config")
	if !poll || rec.status >= 400 {
		Debug.Add("http", "%s %s -> %d (%dms)", r.Method, r.URL.Path, rec.status, time.Since(start).Milliseconds())
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// handleDebugLog dumps the diagnostic trace. Loopback-only like everything
// else; contains no secrets by construction (see debuglog.go header).
func (s *Server) handleDebugLog(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, http.StatusOK, map[string]any{
			"app": "gifsmith", "version": AppVersion,
			"ffmpeg": s.tools.FFmpegVersion, "gifski": s.tools.GifskiVersion,
			"entries": Debug.Dump(),
		})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="gifsmith-server-debug.txt"`)
	_, _ = w.Write([]byte(Debug.Text(s.tools)))
}

// ---------- handlers ----------

// videoPartIndex maps a multipart field name to a clip index: "video" is
// clip 0 (v1.2 compatibility), "video_K" is clip K. Returns -1 for
// non-video fields.
func videoPartIndex(name string) int {
	if name == "video" {
		return 0
	}
	if k, ok := strings.CutPrefix(name, "video_"); ok {
		idx, err := strconv.Atoi(k)
		if err == nil && idx >= 0 && idx < MaxClips {
			return idx
		}
		return -2 // malformed video field: reject loudly, don't silently drop
	}
	return -1
}

// orderedClipPaths validates that uploaded clip indices are contiguous from
// 0 and returns them in order.
func orderedClipPaths(files map[int]string) ([]string, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("missing video part")
	}
	out := make([]string, 0, len(files))
	for i := 0; i < len(files); i++ {
		p, ok := files[i]
		if !ok {
			return nil, fmt.Errorf("clip files are not contiguous: video_%d is missing", i)
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(frontendHTML)
}

// handleFont serves the optional OFL fonts (fetched by scripts/fetch-fonts.sh)
// from s.fontsDir (see WithFontsDir). Allow-list of exact names — no path
// traversal surface.
func (s *Server) handleFont(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	switch name {
	case "Anton-Regular.ttf", "Oswald-SemiBold.woff2", "OFL-Anton.txt", "OFL-Oswald.txt":
	default:
		httpError(w, http.StatusNotFound, "unknown font")
		return
	}
	p := filepath.Join(s.fontsDir, name)
	if _, err := os.Stat(p); err != nil {
		httpError(w, http.StatusNotFound, "font not fetched (run scripts/fetch-fonts.sh)")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, p)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"app":           "gifsmith",
		"version":       AppVersion,
		"ffmpeg":        s.tools.FFmpegVersion,
		"gifski":        s.tools.GifskiVersion,
		"gifskiPresent": s.tools.Gifski != "",
		"warnMB":        s.warnMB,
		"platforms":     Platforms,
		"maxUploadMB":   s.maxUploadMB,
		"limits": map[string]any{
			"maxWidth": MaxWidth, "minWidth": MinWidth,
			"maxFPS": MaxFPS, "minFPS": MinFPS, "maxCues": MaxCues,
			"maxClips": MaxClips,
		},
		"transcribe": func() map[string]any {
			t := map[string]any{
				"available": s.tjobs != nil,
				"local":     true, // v1.4: transcription never leaves the machine
			}
			if s.tjobs != nil {
				t["model"] = filepath.Base(s.tools.WhisperModel)
			}
			return t
		}(),
	})
}

// ---------- transcription handlers ----------

func (s *Server) requireTranscribe(w http.ResponseWriter) bool {
	if s.tjobs == nil {
		httpError(w, http.StatusServiceUnavailable, "transcription is not enabled on this server")
		return false
	}
	return true
}

func (s *Server) handleTranscribeCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireTranscribe(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUploadMB<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		httpError(w, http.StatusBadRequest, "expected multipart/form-data: %v", errBrief(err))
		return
	}

	id, dir, err := s.tjobs.NewJobDir()
	if err != nil {
		if errors.Is(err, ErrTooManyJobs) || errors.Is(err, ErrRateLimited) {
			httpError(w, http.StatusTooManyRequests, "%v", err)
		} else {
			httpError(w, http.StatusInternalServerError, "creating job directory")
		}
		return
	}
	ok := false
	defer func() {
		if !ok {
			s.tjobs.Release(id)
			if err := os.RemoveAll(dir); err != nil {
				Debug.Add("transcribe", "job %s: failed to remove %s after a failed create: %v", id[:8], dir, err)
			}
		}
	}()

	var (
		videoFiles = map[int]string{}
		settings   struct {
			SpeakersExpected int `json:"speakersExpected"`
		}
	)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			httpError(w, http.StatusBadRequest, "reading multipart body: %v", errBrief(err))
			return
		}
		name := part.FormName()
		switch idx := videoPartIndex(name); {
		case name == "settings":
			dec := json.NewDecoder(io.LimitReader(part, 64<<10))
			if err := dec.Decode(&settings); err != nil {
				httpError(w, http.StatusBadRequest, "settings JSON invalid: %v", errBrief(err))
				return
			}
		case idx >= 0:
			if _, dup := videoFiles[idx]; dup {
				httpError(w, http.StatusBadRequest, "duplicate clip field %q", name)
				return
			}
			p := filepath.Join(dir, fmt.Sprintf("input_%02d.video", idx))
			if err := savePart(part, p, s.maxUploadMB<<20); err != nil {
				httpError(w, http.StatusBadRequest, "saving clip %d: %v", idx, errBrief(err))
				return
			}
			videoFiles[idx] = p
		case idx == -2:
			httpError(w, http.StatusBadRequest, "invalid clip field %q", name)
			return
		default:
			_, _ = io.Copy(io.Discard, io.LimitReader(part, 1<<20))
		}
		_ = part.Close()
	}
	clipPaths, err := orderedClipPaths(videoFiles)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if settings.SpeakersExpected < 0 || settings.SpeakersExpected > 10 {
		settings.SpeakersExpected = 0
	}

	clips := make([]TClip, 0, len(clipPaths))
	for i, p := range clipPaths {
		probe, err := Probe(r.Context(), s.tools.FFprobe, p)
		if err != nil {
			httpError(w, http.StatusUnprocessableEntity, "clip %d: %v", i+1, err)
			return
		}
		clips = append(clips, TClip{Path: p, Duration: probe.Duration})
	}

	job, err := s.tjobs.Submit(id, dir, clips, settings.SpeakersExpected)
	if err != nil {
		httpError(w, http.StatusTooManyRequests, "%v", err)
		return
	}
	ok = true
	snap := job.snapshot()
	writeJSON(w, http.StatusAccepted, map[string]any{"id": snap.ID, "state": snap.State, "clipCount": snap.ClipCount})
}

func (s *Server) tjobFromPath(w http.ResponseWriter, r *http.Request) (*TJob, bool) {
	if !s.requireTranscribe(w) {
		return nil, false
	}
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		httpError(w, http.StatusBadRequest, "malformed job id")
		return nil, false
	}
	j, ok := s.tjobs.Get(id)
	if !ok {
		httpError(w, http.StatusNotFound, "no such job")
		return nil, false
	}
	return j, true
}

func (s *Server) handleTranscribeStatus(w http.ResponseWriter, r *http.Request) {
	j, ok := s.tjobFromPath(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, j.snapshot())
}

func (s *Server) handleTranscribeCancel(w http.ResponseWriter, r *http.Request) {
	if !s.requireTranscribe(w) {
		return
	}
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		httpError(w, http.StatusBadRequest, "malformed job id")
		return
	}
	if err := s.tjobs.Cancel(id); err != nil {
		httpError(w, http.StatusNotFound, "no such job")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTranscribeThumb(w http.ResponseWriter, r *http.Request) {
	j, ok := s.tjobFromPath(w, r)
	if !ok {
		return
	}
	t, err := strconv.ParseFloat(r.URL.Query().Get("t"), 64)
	if err != nil || math.IsNaN(t) || math.IsInf(t, 0) {
		httpError(w, http.StatusBadRequest, "invalid t parameter")
		return
	}
	clip := 0 // absent clip param = clip 0 (v1.2 compatibility)
	if cs := r.URL.Query().Get("clip"); cs != "" {
		clip, err = strconv.Atoi(cs)
		if err != nil || clip < 0 || clip >= MaxClips {
			httpError(w, http.StatusBadRequest, "invalid clip parameter")
			return
		}
	}
	p, err := s.tjobs.Thumbnail(r.Context(), j.ID, clip, t)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, p)
}

// createJobRequest is the "settings" JSON field of the multipart body.
// Clips (v1.3) carries per-clip trims, in clip order; the legacy top-level
// TrimStart/TrimEnd pair still works for single-clip requests.
type createJobRequest struct {
	TrimStart float64 `json:"trimStart"`
	TrimEnd   float64 `json:"trimEnd"`
	Clips     []struct {
		TrimStart float64 `json:"trimStart"`
		TrimEnd   float64 `json:"trimEnd"`
	} `json:"clips"`
	Width     int     `json:"width"`
	FPS       float64 `json:"fps"`
	Style     string  `json:"style"`
	BarPx     int     `json:"barPx"`
	BarPos    string  `json:"barPos"`
	BarColor  string  `json:"barColor"`
	Encoder   string  `json:"encoder"`
	Quality   int     `json:"quality"`
	Dither    string  `json:"dither"`
	MaxColors int     `json:"maxColors"`
	Cues      []struct {
		Start float64 `json:"start"`
		End   float64 `json:"end"`
	} `json:"cues"`
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUploadMB<<20)

	mr, err := r.MultipartReader()
	if err != nil {
		httpError(w, http.StatusBadRequest, "expected multipart/form-data: %v", errBrief(err))
		return
	}

	id, dir, err := s.jobs.NewJobDir()
	if err != nil {
		if errors.Is(err, ErrTooManyJobs) {
			httpError(w, http.StatusTooManyRequests, "%v", err)
		} else {
			httpError(w, http.StatusInternalServerError, "creating job directory")
		}
		return
	}
	ok := false
	defer func() {
		if !ok {
			s.jobs.Release(id)
			if err := os.RemoveAll(dir); err != nil {
				Debug.Add("encode", "job %s: failed to remove %s after a failed create: %v", id[:8], dir, err)
			}
		}
	}()

	var (
		req        *createJobRequest
		videoFiles = map[int]string{}
		cueFiles   = map[int]string{}
	)

	// Stream parts to disk; never trust client filenames.
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			httpError(w, http.StatusBadRequest, "reading multipart body: %v", errBrief(err))
			return
		}
		name := part.FormName()
		vidIdx := videoPartIndex(name)
		switch {
		case name == "settings":
			var cj createJobRequest
			dec := json.NewDecoder(io.LimitReader(part, 1<<20))
			if err := dec.Decode(&cj); err != nil {
				httpError(w, http.StatusBadRequest, "settings JSON invalid: %v", errBrief(err))
				return
			}
			req = &cj
		case vidIdx >= 0:
			if _, dup := videoFiles[vidIdx]; dup {
				httpError(w, http.StatusBadRequest, "duplicate clip field %q", name)
				return
			}
			p := filepath.Join(dir, fmt.Sprintf("input_%02d.video", vidIdx))
			if err := savePart(part, p, s.maxUploadMB<<20); err != nil {
				httpError(w, http.StatusBadRequest, "saving clip %d: %v", vidIdx, errBrief(err))
				return
			}
			videoFiles[vidIdx] = p
		case vidIdx == -2:
			httpError(w, http.StatusBadRequest, "invalid clip field %q", name)
			return
		case strings.HasPrefix(name, "cue_"):
			idx, convErr := strconv.Atoi(strings.TrimPrefix(name, "cue_"))
			if convErr != nil || idx < 0 || idx >= MaxCues {
				httpError(w, http.StatusBadRequest, "invalid cue field %q", name)
				return
			}
			p := filepath.Join(dir, fmt.Sprintf("cue_%04d.png", idx))
			if err := savePart(part, p, MaxCuePNG); err != nil {
				httpError(w, http.StatusBadRequest, "saving caption %d: %v", idx, errBrief(err))
				return
			}
			if err := validCuePNG(p); err != nil {
				httpError(w, http.StatusBadRequest, "caption %d: %v", idx, err)
				return
			}
			cueFiles[idx] = p
		default:
			_, _ = io.Copy(io.Discard, io.LimitReader(part, 1<<20))
		}
		_ = part.Close()
	}

	if req == nil {
		httpError(w, http.StatusBadRequest, "missing settings part")
		return
	}
	clipPaths, err := orderedClipPaths(videoFiles)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if len(req.Clips) != 0 && len(req.Clips) != len(clipPaths) {
		httpError(w, http.StatusBadRequest, "clip trim count (%d) != clip file count (%d)", len(req.Clips), len(clipPaths))
		return
	}
	if len(req.Cues) != len(cueFiles) {
		httpError(w, http.StatusBadRequest, "cue metadata count (%d) != caption image count (%d)", len(req.Cues), len(cueFiles))
		return
	}

	ctx := r.Context()
	probes := make([]*ProbeInfo, 0, len(clipPaths))
	for i, p := range clipPaths {
		probe, err := Probe(ctx, s.tools.FFprobe, p)
		if err != nil {
			httpError(w, http.StatusUnprocessableEntity, "clip %d: %v", i+1, err)
			return
		}
		probes = append(probes, probe)
	}

	spec := &JobSpec{
		Width:     req.Width,
		FPS:       req.FPS,
		Style:     req.Style,
		BarPx:     req.BarPx,
		BarPos:    req.BarPos,
		BarColor:  req.BarColor,
		Encoder:   req.Encoder,
		Quality:   req.Quality,
		Dither:    req.Dither,
		MaxColors: req.MaxColors,
	}
	for i, p := range clipPaths {
		cs := ClipSpec{Path: p}
		switch {
		case len(req.Clips) == len(clipPaths):
			cs.TrimStart, cs.TrimEnd = req.Clips[i].TrimStart, req.Clips[i].TrimEnd
		case i == 0: // legacy single-clip fields
			cs.TrimStart, cs.TrimEnd = req.TrimStart, req.TrimEnd
		}
		spec.Clips = append(spec.Clips, cs)
	}
	for i, c := range req.Cues {
		f, okc := cueFiles[i]
		if !okc {
			httpError(w, http.StatusBadRequest, "caption image cue_%d missing", i)
			return
		}
		spec.Cues = append(spec.Cues, CueSpec{Start: c.Start, End: c.End, File: f})
	}

	// Legacy-shape compatibility (review finding MED-1): a request WITHOUT a
	// clips[] array is the v1.2 form, whose cue times were SOURCE-ABSOLUTE
	// seconds rebased against the single trim window. v1.3's Validate expects
	// OUTPUT-timeline times, so a legacy request with trimStart=10 would
	// silently mis-render every caption. Reproduce the v1.2 rebase here.
	if len(req.Clips) == 0 && len(spec.Cues) > 0 {
		spec.Cues = rebaseLegacyCues(spec.Cues, req.TrimStart, req.TrimEnd, probes[0].Duration)
	}

	if err := spec.Validate(probes); err != nil {
		httpError(w, http.StatusUnprocessableEntity, "%v", err)
		return
	}

	job := s.jobs.Submit(id, dir, spec, probes)
	ok = true
	snap := job.snapshot() // never read Job fields unlocked: the encode goroutine is already running
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":    snap.ID,
		"state": snap.State,
		"estimateBytes": EstimateBytes(spec.Width, spec.OutHeight(probes[0]), spec.FPS,
			spec.TotalDuration(), spec.Encoder, spec.Dither),
	})
}

func (s *Server) jobFromPath(w http.ResponseWriter, r *http.Request) (*Job, bool) {
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		httpError(w, http.StatusBadRequest, "malformed job id")
		return nil, false
	}
	j, ok := s.jobs.Get(id)
	if !ok {
		httpError(w, http.StatusNotFound, "no such job")
		return nil, false
	}
	return j, true
}

func (s *Server) handleJobStatus(w http.ResponseWriter, r *http.Request) {
	j, ok := s.jobFromPath(w, r)
	if !ok {
		return
	}
	snap := j.snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"id": snap.ID, "state": snap.State, "progress": snap.Progress,
		"error": snap.Error, "outBytes": snap.OutBytes,
		"warnMB": s.warnMB, "width": snap.Width, "height": snap.Height,
		"fps": snap.FPS, "duration": snap.Duration, "encoder": snap.Encoder,
	})
}

func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		httpError(w, http.StatusBadRequest, "malformed job id")
		return
	}
	if err := s.jobs.Cancel(id); err != nil {
		httpError(w, http.StatusNotFound, "no such job")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleJobResult(w http.ResponseWriter, r *http.Request) {
	j, ok := s.jobFromPath(w, r)
	if !ok {
		return
	}
	p, ready := s.jobs.ResultPath(j.ID)
	if !ready {
		httpError(w, http.StatusConflict, "job is not finished")
		return
	}
	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Content-Disposition", `attachment; filename="gifsmith.gif"`)
	http.ServeFile(w, r, p)
}

// handleJobReveal opens the OS file manager on this job's output GIF —
// the server's own copy in its job directory (the exact bytes the
// download link serves), not wherever the browser ultimately saved a
// downloaded copy, which a web page has no way to know or control. See
// reveal.go for the cross-platform command and the security reasoning for
// why this is safe to expose with the same permissive CORS as everything
// else here (path is always server-derived from the job ID, never
// client-supplied).
func (s *Server) handleJobReveal(w http.ResponseWriter, r *http.Request) {
	j, ok := s.jobFromPath(w, r)
	if !ok {
		return
	}
	p, ready := s.jobs.ResultPath(j.ID)
	if !ready {
		httpError(w, http.StatusConflict, "job is not finished")
		return
	}
	if err := s.jobs.CheckReveal(j.ID); err != nil {
		httpError(w, http.StatusTooManyRequests, "%v", err)
		return
	}
	if err := revealInFileManager(r.Context(), p); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", errBrief(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": p})
}

// ---------- helpers ----------

// rebaseLegacyCues converts v1.2 SOURCE-ABSOLUTE cue times into v1.3
// OUTPUT-timeline times against a single trim window: clamp to [ts,te],
// rebase to 0, drop cues fully outside — exactly the v1.2 Validate
// behavior, so old clients/scripts keep rendering captions correctly.
// NaN windows pass through untouched for Validate to reject with a proper
// error.
func rebaseLegacyCues(cues []CueSpec, trimStart, trimEnd, probeDur float64) []CueSpec {
	ts, te := trimStart, trimEnd
	if ts < 0 || math.IsNaN(ts) || math.IsInf(ts, 0) {
		ts = 0
	}
	if te <= 0 || te > probeDur || math.IsNaN(te) || math.IsInf(te, 0) {
		te = probeDur
	}
	kept := cues[:0]
	for _, q := range cues {
		if math.IsNaN(q.Start) || math.IsNaN(q.End) {
			kept = append(kept, q)
			continue
		}
		if q.End < ts || q.Start > te {
			continue
		}
		q.Start = math.Max(q.Start, ts) - ts
		q.End = math.Min(q.End, te) - ts
		if q.End > q.Start {
			kept = append(kept, q)
		}
	}
	return kept
}

// savePart streams a multipart part to path, failing if it exceeds max bytes.
func savePart(part *multipart.Part, path string, max int64) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(part, max+1))
	if err != nil {
		return err
	}
	if n > max {
		return fmt.Errorf("part exceeds %d byte limit", max)
	}
	if n == 0 {
		return fmt.Errorf("empty upload")
	}
	return nil
}

// validCuePNG checks the PNG signature AND the declared IHDR width/height
// against MaxPixels before ffmpeg ever decodes the file. The signature
// alone doesn't bound decode-time memory: ffmpeg allocates the full raster
// for the file's *declared* dimensions regardless of on-disk (compressed)
// byte size, so a small file can declare an enormous canvas and force a
// multi-GB allocation well before MaxCuePNG's byte cap is anywhere close.
func validCuePNG(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("not a PNG")
	}
	defer f.Close()
	hdr := make([]byte, 24)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return fmt.Errorf("not a PNG")
	}
	if string(hdr[:8]) != "\x89PNG\r\n\x1a\n" || string(hdr[12:16]) != "IHDR" {
		return fmt.Errorf("not a PNG")
	}
	w := binary.BigEndian.Uint32(hdr[16:20])
	h := binary.BigEndian.Uint32(hdr[20:24])
	if w == 0 || h == 0 || uint64(w)*uint64(h) > MaxPixels {
		return fmt.Errorf("declared %dx%d exceeds the %dMP limit", w, h, MaxPixels/1_000_000)
	}
	return nil
}

func cleanupMultipart(r *http.Request) {
	if r.MultipartForm != nil {
		if err := r.MultipartForm.RemoveAll(); err != nil {
			Debug.Add("http", "cleanup: failed to remove multipart temp files for %s: %v", r.URL.Path, err)
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	if err := enc.Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}

func httpError(w http.ResponseWriter, code int, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	Debug.Add("http-error", "%d: %s", code, msg)
	writeJSON(w, code, map[string]string{"error": msg})
}

func errBrief(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// loopbackHost reports whether a request Host header names loopback.
func loopbackHost(host string) bool {
	h := host
	if i := strings.LastIndex(h, ":"); i != -1 && !strings.HasSuffix(h, "]") && strings.Count(h, ":") == 1 {
		h = h[:i] // strip :port (IPv4/name form)
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if i := strings.LastIndex(host, "]:"); i != -1 { // [::1]:port form
		h = strings.TrimPrefix(host[:i], "[")
	}
	switch h {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// ListenLoopback starts the server on 127.0.0.1:port.
func ListenLoopback(port int, handler http.Handler) (*http.Server, string, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv, "http://" + addr + "/", nil
}
