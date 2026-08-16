//go:build ignore

// Moved here, not deleted, when subtitle-file upload was removed (the
// operator's own convention for flagged-for-deletion files). Excluded from
// the build via this tag -- retained Go source under a non-package
// directory still gets type-checked by `go build ./...`/`go vet ./...`
// otherwise, which would break the build for a file nothing imports.

package main

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

// postSubtitle POSTs a trivial, always-valid SRT cue to /api/subtitles.
func postSubtitle(t *testing.T, url string) *http.Response {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	fw, _ := mw.CreateFormFile("file", "s.srt")
	_, _ = fw.Write([]byte("1\n00:00:01,000 --> 00:00:02,000\nHi\n\n"))
	mw.Close()
	resp, err := http.Post(url+"/api/subtitles", mw.FormDataContentType(), &b)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestSubtitlesRateLimit: POST /api/subtitles had no rate limit at all
// (adversarial-review finding) unlike every other resource-touching
// endpoint. subtitleRateMaxPerWindow calls must succeed; the next one within
// the window must 429.
func TestSubtitlesRateLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	tools := &Tools{}
	jm := NewJobManager(t.TempDir(), tools)
	defer jm.Shutdown()
	srv := NewServer(tools, jm, 512, DefaultWarnMB)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	for i := 0; i < subtitleRateMaxPerWindow; i++ {
		resp := postSubtitle(t, ts.URL)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("subtitle post #%d: HTTP %d, want 200", i, resp.StatusCode)
		}
	}
	resp := postSubtitle(t, ts.URL)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("subtitle post beyond the window: HTTP %d, want %d", resp.StatusCode, http.StatusTooManyRequests)
	}
}
