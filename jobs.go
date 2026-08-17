package main

// jobs.go — in-memory job manager. One encode runs at a time (this is a
// single-user local tool); queued jobs wait on a semaphore. Every job owns
// a private directory under the server's temp root, deleted on cancel and
// swept after a TTL.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type JobState string

const (
	JobQueued    JobState = "queued"
	JobEncoding  JobState = "encoding"
	JobDone      JobState = "done"
	JobError     JobState = "error"
	JobCancelled JobState = "cancelled"
)

type Job struct {
	ID       string   `json:"id"`
	State    JobState `json:"state"`
	Progress float64  `json:"progress"` // 0..1
	Error    string   `json:"error,omitempty"`
	OutBytes int64    `json:"outBytes,omitempty"`
	Width    int      `json:"width"`
	Height   int      `json:"height"`
	FPS      float64  `json:"fps"`
	Duration float64  `json:"duration"` // encoded seconds
	Created  int64    `json:"created"`  // unix seconds

	dir     string
	outPath string
	timeout time.Duration
	cancel  context.CancelFunc
	mu      sync.Mutex
}

func (j *Job) snapshot() Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	return Job{
		ID: j.ID, State: j.State, Progress: j.Progress, Error: j.Error,
		OutBytes: j.OutBytes, Width: j.Width,
		Height: j.Height, FPS: j.FPS, Duration: j.Duration, Created: j.Created,
	}
}

type JobManager struct {
	mu       sync.Mutex
	jobs     map[string]*Job
	reserved map[string]bool // ids handed out by NewJobDir, not yet Submitted
	tmpRoot  string
	tools    *Tools
	sem      chan struct{} // encode concurrency = 1
	ttl      time.Duration
	stop     chan struct{}
}

func NewJobManager(tmpRoot string, tools *Tools) *JobManager {
	m := &JobManager{
		jobs:     make(map[string]*Job),
		reserved: make(map[string]bool),
		tmpRoot:  tmpRoot,
		tools:    tools,
		sem:      make(chan struct{}, 1),
		ttl:      2 * time.Hour,
		stop:     make(chan struct{}),
	}
	go m.sweep()
	return m
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error()) // fail closed
	}
	return hex.EncodeToString(b)
}

// MaxLiveJobs bounds job-table and temp-dir growth (review finding: without
// a cap, a hostile local page could accumulate 2h-TTL upload dirs).
const MaxLiveJobs = 16

// Encode jobs previously had no wall-clock timeout at all: a pathologically
// slow or huge input could hold the single encode semaphore (sem, below)
// indefinitely, blocking every other queued job until the client that
// created it happened to issue a DELETE. This budget scales with the
// requested output duration (bounded), the same shape as TranscribeManager's
// per-clip-scaled timeout.
const (
	encodeBaseTimeout = 5 * time.Minute
	encodePerSecond   = 3 * time.Second
	encodeMaxTimeout  = 30 * time.Minute
)

func encodeTimeout(totalDuration float64) time.Duration {
	t := encodeBaseTimeout + time.Duration(totalDuration*float64(encodePerSecond))
	if t > encodeMaxTimeout {
		t = encodeMaxTimeout
	}
	return t
}

// ErrTooManyJobs is returned by NewJobDir at the MaxLiveJobs cap.
var ErrTooManyJobs = fmt.Errorf("too many active jobs; wait for one to finish or delete finished jobs")

// NewJobDir reserves a slot and creates the private directory for an
// incoming upload. The reservation closes the check-then-act gap between
// this cap check and Submit (TOCTOU review finding): N concurrent multi-GB
// uploads each hold a counted slot while streaming. Callers MUST either
// Submit(id, …) or Release(id).
func (m *JobManager) NewJobDir() (string, string, error) {
	m.mu.Lock()
	if len(m.jobs)+len(m.reserved) >= MaxLiveJobs {
		m.mu.Unlock()
		return "", "", ErrTooManyJobs
	}
	id := newID()
	m.reserved[id] = true
	m.mu.Unlock()

	dir := filepath.Join(m.tmpRoot, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.Release(id)
		return "", "", err
	}
	return id, dir, nil
}

// Release frees a reservation whose upload failed before Submit.
func (m *JobManager) Release(id string) {
	m.mu.Lock()
	delete(m.reserved, id)
	m.mu.Unlock()
}

// Submit registers and starts a job. spec must already be validated.
// probes are per-clip, in clip order.
func (m *JobManager) Submit(id, dir string, spec *JobSpec, probes []*ProbeInfo) *Job {
	ctx, cancel := context.WithCancel(context.Background())
	total := spec.TotalDuration()
	j := &Job{
		ID:       id,
		State:    JobQueued,
		Width:    spec.Width,
		Height:   spec.OutHeight(probes[0]),
		FPS:      spec.FPS,
		Duration: total,
		Created:  time.Now().Unix(),
		dir:      dir,
		outPath:  filepath.Join(dir, "out.gif"),
		timeout:  encodeTimeout(total),
		cancel:   cancel,
	}
	m.mu.Lock()
	delete(m.reserved, id) // reservation becomes a real job
	m.jobs[id] = j
	m.mu.Unlock()

	go m.run(ctx, j, spec, probes)
	return j
}

func (m *JobManager) run(parent context.Context, j *Job, spec *JobSpec, probes []*ProbeInfo) {
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-parent.Done():
		j.mu.Lock()
		j.State = JobCancelled
		j.mu.Unlock()
		Debug.Add("encode", "job %s cancelled while queued", j.ID[:8])
		return
	}
	// The wall-clock budget starts NOW, not at Submit — time spent queued
	// behind another job doesn't eat into it.
	ctx, tcancel := context.WithTimeout(parent, j.timeout)
	defer tcancel()

	j.mu.Lock()
	j.State = JobEncoding
	j.mu.Unlock()
	Debug.Add("encode", "job %s start: %dx%d @%gfps %.1fs clips=%d style=%s cues=%d",
		j.ID[:8], spec.Width, spec.OutHeight(probes[0]), spec.FPS,
		spec.TotalDuration(), len(spec.Clips), spec.Style, len(spec.Cues))
	started := time.Now()

	err := Run(ctx, m.tools, j.dir, spec, probes, j.outPath, func(f float64) {
		j.mu.Lock()
		if f > j.Progress {
			j.Progress = f
		}
		j.mu.Unlock()
	})

	j.mu.Lock()
	defer j.mu.Unlock()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		j.State = JobError
		j.Error = fmt.Sprintf("encode exceeded the %d-minute limit", int(j.timeout.Minutes()))
		Debug.Add("encode", "job %s TIMED OUT after %.1fs", j.ID[:8], time.Since(started).Seconds())
		return
	}
	if ctx.Err() != nil {
		j.State = JobCancelled
		Debug.Add("encode", "job %s cancelled after %.1fs", j.ID[:8], time.Since(started).Seconds())
		return
	}
	if err != nil {
		j.State = JobError
		j.Error = err.Error()
		Debug.Add("encode", "job %s FAILED after %.1fs: %s", j.ID[:8], time.Since(started).Seconds(), j.Error)
		return
	}
	fi, statErr := os.Stat(j.outPath)
	if statErr != nil {
		j.State = JobError
		j.Error = "encoder reported success but no output file was produced"
		Debug.Add("encode", "job %s FAILED: no output file", j.ID[:8])
		return
	}
	j.OutBytes = fi.Size()
	j.Progress = 1
	j.State = JobDone
	Debug.Add("encode", "job %s done in %.1fs: %.2f MB", j.ID[:8], time.Since(started).Seconds(), float64(j.OutBytes)/1048576)
}

func (m *JobManager) Get(id string) (*Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	return j, ok
}

// Cancel stops a running job and deletes its directory.
func (m *JobManager) Cancel(id string) error {
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
	// Give the encoder a moment to die before removing files; RemoveAll on
	// open files is fine on POSIX regardless.
	go func() {
		time.Sleep(500 * time.Millisecond)
		if err := os.RemoveAll(j.dir); err != nil {
			Debug.Add("encode", "job %s: cleanup failed to remove %s: %v", id[:8], j.dir, err)
		}
	}()
	return nil
}

// ResultPath returns the output path for a completed job.
func (m *JobManager) ResultPath(id string) (string, bool) {
	j, ok := m.Get(id)
	if !ok {
		return "", false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.State != JobDone {
		return "", false
	}
	return j.outPath, true
}

// sweep deletes expired jobs and their directories.
func (m *JobManager) sweep() {
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-tick.C:
		}
		cutoff := time.Now().Add(-m.ttl).Unix()
		var doomed []string // delete dirs outside the lock (disk I/O can be slow)
		m.mu.Lock()
		for id, j := range m.jobs {
			j.mu.Lock()
			expired := j.Created < cutoff
			running := j.State == JobEncoding || j.State == JobQueued
			j.mu.Unlock()
			if expired && !running {
				delete(m.jobs, id)
				doomed = append(doomed, j.dir)
			}
		}
		m.mu.Unlock()
		for _, d := range doomed {
			if err := os.RemoveAll(d); err != nil {
				Debug.Add("encode", "TTL sweep: failed to remove %s: %v", d, err)
			}
		}
	}
}

// Shutdown cancels everything and removes the temp root.
func (m *JobManager) Shutdown() {
	close(m.stop)
	m.mu.Lock()
	for _, j := range m.jobs {
		j.cancel()
	}
	m.mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	if err := os.RemoveAll(m.tmpRoot); err != nil {
		Debug.Add("encode", "shutdown: failed to remove tmp root %s: %v", m.tmpRoot, err)
	}
}
