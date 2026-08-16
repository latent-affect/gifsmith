package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stubReveal replaces revealInFileManager for the duration of the test,
// recording every path it was asked to reveal instead of popping a real
// GUI window (which `go test` must never do).
func stubReveal(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	orig := revealInFileManager
	revealInFileManager = func(ctx context.Context, path string) error {
		calls = append(calls, path)
		return nil
	}
	t.Cleanup(func() { revealInFileManager = orig })
	return &calls
}

func createAndFinishJob(t *testing.T, ts *httptest.Server, tools *Tools, dir string) (id, outPath string) {
	t.Helper()
	video := makeSampleVideo(t, tools, dir)
	cuePath := makeCuePNG(t, dir, "cue_reveal.png", 240, 134, color.RGBA{0, 255, 0, 255})
	var jb bytes.Buffer
	jw := multipart.NewWriter(&jb)
	settings := map[string]any{
		"trimStart": 0.0, "trimEnd": 2.0, "width": 160, "fps": 10,
		"style": "classic", "encoder": "ffmpeg", "dither": "bayer",
		"cues": []map[string]float64{{"start": 0.2, "end": 1.0}},
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
		t.Fatalf("job create: HTTP %d", resp.StatusCode)
	}
	var created struct{ ID string }
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	deadline := time.Now().Add(2 * time.Minute)
	for {
		if time.Now().After(deadline) {
			t.Fatal("job did not finish in time")
		}
		r, _ := http.Get(ts.URL + "/api/jobs/" + created.ID)
		var st struct{ State, Error string }
		_ = json.NewDecoder(r.Body).Decode(&st)
		r.Body.Close()
		if st.State == "done" {
			break
		}
		if st.State == "error" {
			t.Fatalf("job failed: %s", st.Error)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return created.ID, filepath.Join(dir, "tmp", created.ID, "out.gif")
}

func TestRevealFinishedJob(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := testTools(t)
	dir := t.TempDir()
	calls := stubReveal(t)

	jm := NewJobManager(filepath.Join(dir, "tmp"), tools)
	srv := NewServer(tools, jm, 512, DefaultWarnMB)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	id, wantPath := createAndFinishJob(t, ts, tools, dir)

	resp, err := http.Post(ts.URL+"/api/jobs/"+id+"/reveal", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("reveal: HTTP %d %s", resp.StatusCode, body)
	}
	var out struct{ Path string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Path != wantPath {
		t.Errorf("revealed path = %q, want %q", out.Path, wantPath)
	}
	if len(*calls) != 1 || (*calls)[0] != wantPath {
		t.Errorf("revealInFileManager calls = %v, want exactly [%q]", *calls, wantPath)
	}
}

func TestRevealNotReadyRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := testTools(t)
	dir := t.TempDir()
	calls := stubReveal(t)

	jm := NewJobManager(filepath.Join(dir, "tmp"), tools)
	srv := NewServer(tools, jm, 512, DefaultWarnMB)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	video := makeSampleVideo(t, tools, dir)
	cuePath := makeCuePNG(t, dir, "cue_notready.png", 240, 134, color.RGBA{0, 0, 255, 255})
	var jb bytes.Buffer
	jw := multipart.NewWriter(&jb)
	settings := map[string]any{
		"trimStart": 0.0, "trimEnd": 2.0, "width": 160, "fps": 10,
		"style": "classic", "encoder": "ffmpeg", "dither": "bayer",
		"cues": []map[string]float64{{"start": 0.2, "end": 1.0}},
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
	var created struct{ ID string }
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	// Reveal immediately, before the job can possibly have finished.
	rr, err := http.Post(ts.URL+"/api/jobs/"+created.ID+"/reveal", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	rr.Body.Close()
	if rr.StatusCode != http.StatusConflict {
		t.Errorf("reveal on unfinished job: HTTP %d, want %d", rr.StatusCode, http.StatusConflict)
	}
	if len(*calls) != 0 {
		t.Errorf("revealInFileManager should not have been called: %v", *calls)
	}
}

// TestRevealCooldown is a pure unit test against JobManager.CheckReveal
// directly (white-box: same package, reaches into the unexported jobs map)
// — no HTTP, no ffmpeg, fast. Confirms the per-job cooldown found missing
// by the adversarial review: an immediate second reveal on the same job
// must be rejected.
func TestRevealCooldown(t *testing.T) {
	jm := NewJobManager(t.TempDir(), &Tools{})
	defer jm.Shutdown()
	j := &Job{ID: "cooldown-job", State: JobDone, outPath: "/x/out.gif", cancel: func() {}}
	jm.mu.Lock()
	jm.jobs[j.ID] = j
	jm.mu.Unlock()

	if err := jm.CheckReveal(j.ID); err != nil {
		t.Fatalf("first CheckReveal: %v, want nil", err)
	}
	if err := jm.CheckReveal(j.ID); err != ErrRevealCooldown {
		t.Fatalf("second CheckReveal (immediate): err = %v, want ErrRevealCooldown", err)
	}
}

// TestRevealGlobalRateLimit confirms the round-robin-across-jobs variant the
// review's follow-up design pass identified: a per-job cooldown alone isn't
// enough, since a finished job stays revealable for up to its 2h TTL and
// MaxLiveJobs gives room to spread calls across several distinct jobs.
func TestRevealGlobalRateLimit(t *testing.T) {
	jm := NewJobManager(t.TempDir(), &Tools{})
	defer jm.Shutdown()

	mkJob := func(id string) {
		j := &Job{ID: id, State: JobDone, outPath: "/x/" + id + ".gif", cancel: func() {}}
		jm.mu.Lock()
		jm.jobs[id] = j
		jm.mu.Unlock()
	}

	for i := 0; i < revealMaxPerWindow; i++ {
		id := fmt.Sprintf("rate-job-%d", i)
		mkJob(id)
		if err := jm.CheckReveal(id); err != nil {
			t.Fatalf("CheckReveal on distinct job #%d: %v, want nil", i, err)
		}
	}
	mkJob("rate-job-extra")
	if err := jm.CheckReveal("rate-job-extra"); err != ErrRevealRateLimited {
		t.Fatalf("CheckReveal on a %dth distinct job within the window: err = %v, want ErrRevealRateLimited",
			revealMaxPerWindow+1, err)
	}
}

// TestRevealCooldownOverHTTP is the real-HTTP counterpart to
// TestRevealCooldown: hits the actual /reveal endpoint twice on one real
// finished job and confirms the 2nd is a genuine 429, not just asserted
// against the manager directly.
func TestRevealCooldownOverHTTP(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := testTools(t)
	dir := t.TempDir()
	calls := stubReveal(t)

	jm := NewJobManager(filepath.Join(dir, "tmp"), tools)
	srv := NewServer(tools, jm, 512, DefaultWarnMB)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	id, _ := createAndFinishJob(t, ts, tools, dir)

	resp1, err := http.Post(ts.URL+"/api/jobs/"+id+"/reveal", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first reveal: HTTP %d, want 200", resp1.StatusCode)
	}

	resp2, err := http.Post(ts.URL+"/api/jobs/"+id+"/reveal", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second immediate reveal: HTTP %d, want 429", resp2.StatusCode)
	}
	if len(*calls) != 1 {
		t.Errorf("revealInFileManager calls = %d, want exactly 1 (cooldown should have blocked the 2nd)", len(*calls))
	}
}

// TestRevealGlobalRateLimitOverHTTP is the real-HTTP counterpart to
// TestRevealGlobalRateLimit: creates revealMaxPerWindow+1 REAL jobs (small,
// fast encodes) via the actual API and reveals each exactly once, confirming
// the round-robin-across-jobs variant of the attack — which a per-job
// cooldown alone would not stop — is genuinely blocked end-to-end, not just
// at the JobManager level (adversarial re-review finding: this coverage gap
// meant the global window's real HTTP wiring, as opposed to its logic, was
// unverified).
func TestRevealGlobalRateLimitOverHTTP(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := testTools(t)
	dir := t.TempDir()
	calls := stubReveal(t)

	jm := NewJobManager(filepath.Join(dir, "tmp"), tools)
	srv := NewServer(tools, jm, 512, DefaultWarnMB)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	n := revealMaxPerWindow + 1
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id, _ := createAndFinishJob(t, ts, tools, dir)
		ids[i] = id
	}

	for i, id := range ids {
		resp, err := http.Post(ts.URL+"/api/jobs/"+id+"/reveal", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if i < revealMaxPerWindow {
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("reveal on distinct job #%d: HTTP %d, want 200", i, resp.StatusCode)
			}
		} else {
			if resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("reveal on the %dth distinct job within the window: HTTP %d, want 429", i+1, resp.StatusCode)
			}
		}
	}
	if len(*calls) != revealMaxPerWindow {
		t.Errorf("revealInFileManager calls = %d, want exactly %d (the global window should have blocked the last one)",
			len(*calls), revealMaxPerWindow)
	}
}

func TestRevealHostileIDs(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := testTools(t)
	dir := t.TempDir()
	stubReveal(t)

	jm := NewJobManager(filepath.Join(dir, "tmp"), tools)
	srv := NewServer(tools, jm, 512, DefaultWarnMB)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	for _, id := range []string{"../../etc/passwd", "zzzz", "'; DROP TABLE jobs;--"} {
		resp, err := http.Post(ts.URL+"/api/jobs/"+id+"/reveal", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("reveal(%q) = HTTP %d, want a 4xx rejection", id, resp.StatusCode)
		}
	}
}
