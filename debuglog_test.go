package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDebugLogRingWraparound(t *testing.T) {
	d := NewDebugLog(16)
	for i := 0; i < 40; i++ {
		d.Add("test", "entry %d", i)
	}
	got := d.Dump()
	if len(got) != 16 {
		t.Fatalf("dump len = %d, want 16", len(got))
	}
	if got[0].Msg != "entry 24" || got[15].Msg != "entry 39" {
		t.Errorf("wrong window: first=%q last=%q", got[0].Msg, got[15].Msg)
	}
	for i := 1; i < len(got); i++ {
		if got[i].T.Before(got[i-1].T) {
			t.Errorf("entries out of order at %d", i)
		}
	}
}

func TestDebugLogClipsLongMessages(t *testing.T) {
	d := NewDebugLog(16)
	d.Add("test", "%s", strings.Repeat("x", 5000))
	if got := d.Dump()[0].Msg; len(got) > 700 {
		t.Errorf("message not clipped: %d bytes", len(got))
	}
}

func TestDebugLogEndpoint(t *testing.T) {
	tools := &Tools{FFmpegVersion: "test-ffmpeg", GifskiVersion: "test-gifski"}
	jm := &JobManager{jobs: map[string]*Job{}, reserved: map[string]bool{}, stop: make(chan struct{})}
	srv := NewServer(tools, jm, 16, DefaultWarnMB)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	Debug.Add("test-marker", "endpoint smoke %d", 42)
	r, err := http.Get(ts.URL + "/api/debug/log")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	body := make([]byte, 1<<20)
	n, _ := r.Body.Read(body)
	txt := string(body[:n])
	if r.StatusCode != 200 || !strings.Contains(txt, "endpoint smoke 42") {
		t.Fatalf("debug log endpoint: HTTP %d, marker present=%v", r.StatusCode, strings.Contains(txt, "endpoint smoke 42"))
	}
	if !strings.Contains(txt, "test-ffmpeg") {
		t.Errorf("environment header missing")
	}
	if strings.Contains(txt, "TESTKEY") || strings.Contains(txt, "SECRET") {
		t.Errorf("secret-looking content in debug log")
	}
}
