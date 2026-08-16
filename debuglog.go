package main

// debuglog.go — in-memory diagnostic trace for "what just happened?"
// moments. A fixed-size ring buffer records timestamped events from every
// subsystem (HTTP requests, encode/transcribe lifecycles, tool errors);
// GET /api/debug/log dumps it for the frontend's "Export debug log"
// button, which bundles it with the client-side event log.
//
// Redaction rule: NOTHING secret is ever passed to Add. API keys are never
// logged (call sites log only their presence); the only identifiers here
// are server-generated job ids and tool stderr excerpts.

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
)

type DebugEntry struct {
	T    time.Time `json:"t"`
	Comp string    `json:"comp"`
	Msg  string    `json:"msg"`
}

type DebugLog struct {
	mu   sync.Mutex
	buf  []DebugEntry
	next int
	full bool
}

const debugLogSize = 2000

// Debug is the process-wide trace buffer.
var Debug = NewDebugLog(debugLogSize)

func NewDebugLog(n int) *DebugLog {
	if n < 16 {
		n = 16
	}
	return &DebugLog{buf: make([]DebugEntry, n)}
}

// Add records one event. Format args are rendered immediately (values may
// be mutated later by callers); messages are clipped to keep the buffer
// bounded even with pathological stderr excerpts.
func (d *DebugLog) Add(comp, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if len(msg) > 600 {
		msg = msg[:600] + "…"
	}
	e := DebugEntry{T: time.Now().UTC(), Comp: comp, Msg: msg}
	d.mu.Lock()
	d.buf[d.next] = e
	d.next++
	if d.next == len(d.buf) {
		d.next = 0
		d.full = true
	}
	d.mu.Unlock()
}

// Dump returns entries oldest-first.
func (d *DebugLog) Dump() []DebugEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []DebugEntry
	if d.full {
		out = append(out, d.buf[d.next:]...)
	}
	out = append(out, d.buf[:d.next]...)
	// Copy so callers can't observe later ring writes.
	cp := make([]DebugEntry, len(out))
	copy(cp, out)
	return cp
}

// Text renders the dump as a plain-text report with an environment header.
func (d *DebugLog) Text(tools *Tools) string {
	var b strings.Builder
	fmt.Fprintf(&b, "GIFsmith %s debug log — generated %s\n", AppVersion, time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "os/arch : %s/%s · go %s\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
	if tools != nil {
		fmt.Fprintf(&b, "ffmpeg  : %s\n", tools.FFmpegVersion)
		if tools.GifskiVersion != "" {
			fmt.Fprintf(&b, "gifski  : %s\n", tools.GifskiVersion)
		}
	}
	b.WriteString(strings.Repeat("-", 72) + "\n")
	for _, e := range d.Dump() {
		fmt.Fprintf(&b, "%s [%s] %s\n", e.T.Format("15:04:05.000"), e.Comp, e.Msg)
	}
	return b.String()
}
