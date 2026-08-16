package main

// GIFsmith — local video→GIF converter with meme-style timed captions.
//
// Usage:
//
//	gifsmith [-port 8437] [-tools ./bin] [-max-upload-mb 2048] [-warn-mb 30.2]
//
// The frontend is served at http://127.0.0.1:<port>/ and the identical file
// (frontend/index.html) also works opened directly from disk.

import (
	_ "embed"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

//go:embed frontend/index.html
var frontendHTML []byte

func main() {
	port := flag.Int("port", 8437, "port to listen on (loopback only)")
	toolsDir := flag.String("tools", defaultToolsDir(), "directory containing ffmpeg/ffprobe/gifski binaries (falls back to $PATH)")
	fontsDir := flag.String("fonts", defaultFontsDir(), "directory containing fetched OFL fonts (see scripts/fetch-fonts.sh)")
	tmpDir := flag.String("tmp", "", "working directory for jobs (default: OS temp)")
	maxUploadMB := flag.Int64("max-upload-mb", 2048, "maximum upload size in MB")
	warnMB := flag.Float64("warn-mb", DefaultWarnMB, "output-size warning threshold in MB (default = mean of verified GIF-autoplay platform limits)")
	whisperModel := flag.String("whisper-model", "", "path to a whisper.cpp ggml model file (default: models/ggml-*.bin next to the tools dir)")
	flag.Parse()

	tools, err := findTools(*toolsDir, *whisperModel)
	if err != nil {
		log.Fatalf("toolchain: %v", err)
	}
	log.Printf("ffmpeg : %s", tools.FFmpegVersion)
	if tools.Gifski != "" {
		log.Printf("gifski : %s", tools.GifskiVersion)
	} else {
		log.Printf("gifski : not found — the high-quality encoder is disabled (run scripts/setup-tools.sh); the ffmpeg encoder still works")
	}
	if tools.HasTranscribe() {
		log.Printf("transcribe: local (whisper model %s + sherpa-onnx diarization) — nothing leaves this machine",
			filepath.Base(tools.WhisperModel))
	} else {
		log.Printf("transcribe: disabled — whisper/diarization tools or models missing (run scripts/setup-tools.sh --transcribe)")
	}

	root := *tmpDir
	if root == "" {
		root = filepath.Join(os.TempDir(), "gifsmith")
	}
	root = filepath.Join(root, fmt.Sprintf("run-%d", os.Getpid()))
	if err := os.MkdirAll(root, 0o700); err != nil {
		log.Fatalf("temp dir: %v", err)
	}

	jobs := NewJobManager(root, tools)
	var tjobs *TranscribeManager
	app := NewServer(tools, jobs, *maxUploadMB, *warnMB).WithFontsDir(*fontsDir)
	if tools.HasTranscribe() {
		tjobs = NewTranscribeManager(root, tools)
		app = app.WithTranscribe(tjobs)
	}
	srv, url, err := ListenLoopback(*port, app)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Printf("shutting down…")
		if tjobs != nil {
			tjobs.Shutdown()
		}
		jobs.Shutdown()
		os.Exit(0)
	}()

	log.Printf("GIFsmith %s — open %s (or open frontend/index.html directly)", AppVersion, url)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func defaultToolsDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "bin")
	}
	return "bin"
}

// defaultFontsDir mirrors defaultToolsDir (cross-unit-contract review
// finding: fonts/ used to resolve against the process's CWD while bin/
// resolved against the executable's own directory — two different
// conventions for what should be one).
func defaultFontsDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "fonts")
	}
	return "fonts"
}

// findTools resolves ffmpeg, ffprobe and gifski: preferred from dir, else
// $PATH. gifski is optional; ffmpeg+ffprobe are required. The local
// transcription toolchain (whisper-cli + sherpa-onnx diarization + model
// files under dir/models) is optional as a set — if any piece is missing
// the Transcribe tab is disabled and everything else still works.
func findTools(dir, whisperModel string) (*Tools, error) {
	find := func(name string) string {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
		return ""
	}
	fileIf := func(p string) string {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
		return ""
	}
	t := &Tools{
		FFmpeg:   find("ffmpeg"),
		FFprobe:  find("ffprobe"),
		Gifski:   find("gifski"),
		Whisper:  find("whisper-cli"),
		Diarizer: find("sherpa-onnx-offline-speaker-diarization"),
	}
	if t.FFmpeg == "" || t.FFprobe == "" {
		return nil, fmt.Errorf("ffmpeg/ffprobe not found in %q or $PATH — run scripts/setup-tools.sh", dir)
	}
	models := filepath.Join(dir, "models")
	t.DiarSegModel = fileIf(filepath.Join(models, "sherpa-segmentation.onnx"))
	t.DiarEmbModel = fileIf(filepath.Join(models, "sherpa-embedding.onnx"))
	switch {
	case whisperModel != "":
		t.WhisperModel = fileIf(whisperModel)
		if t.WhisperModel == "" {
			return nil, fmt.Errorf("-whisper-model %q: file not found", whisperModel)
		}
	default:
		// Prefer ggml-base.bin (the setup-script default); otherwise the
		// lexicographically first match, so the pick is deterministic.
		if p := fileIf(filepath.Join(models, "ggml-base.bin")); p != "" {
			t.WhisperModel = p
		} else if matches, _ := filepath.Glob(filepath.Join(models, "ggml-*.bin")); len(matches) > 0 {
			sort.Strings(matches)
			t.WhisperModel = matches[0]
		}
	}
	t.FFmpegVersion = versionOf(t.FFmpeg, "-version")
	if t.Gifski != "" {
		t.GifskiVersion = versionOf(t.Gifski, "--version")
	}
	return t, nil
}

func versionOf(bin string, arg string) string {
	out, err := exec.Command(bin, arg).Output()
	if err != nil {
		return "unknown"
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	if len(line) > 120 {
		line = line[:120]
	}
	return line
}
