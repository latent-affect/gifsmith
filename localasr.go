package main

// localasr.go — fully local transcription + speaker diarization (v1.4).
//
// This replaced the AssemblyAI client (assemblyai.go, removed): audio never
// leaves the machine. Two pinned subprocess binaries do the work, following
// the exact pattern already established for ffmpeg/gifski — separate
// executables, never linked, fetched checksum-pinned by scripts/setup-tools.sh:
//
//   - whisper-cli (whisper.cpp, MIT): speech→text with word-level timestamps.
//     Invoked with `-ml 1 -sow` so every output segment is one word, which
//     keeps the JSON parsing trivial and the word timing explicit.
//   - sherpa-onnx-offline-speaker-diarization (sherpa-onnx, Apache-2.0):
//     who-spoke-when segments via pyannote-segmentation-3.0 (MIT) +
//     NeMo TitaNet-small (CC-BY-4.0) ONNX models. All model files come from
//     k2-fsa's GitHub releases — no HuggingFace account or token anywhere.
//
// Determinism note (docs/ARCHITECTURE.md has the full story): both tools are
// reproducible run-to-run on the same machine with the same model files and
// thread count — verified by double-run in this repo's tooling — but neural
// inference is NOT bit-exact across different hardware/SIMD paths the way
// the GIF encode path is. That is why the transcription feature is described
// as "reproducible per machine+model", never "deterministic" in the encode
// path's byte-exact sense.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// SpeakerSegment is one diarization result: speaker N spoke [Start,End).
type SpeakerSegment struct {
	Start, End float64
	Speaker    int
}

// spokenWord is one transcribed word with its assigned speaker label.
type spokenWord struct {
	WordStamp
	Speaker string
}

// ---------- whisper.cpp ----------

// whisperJSON mirrors the parts of whisper-cli's -oj output we consume.
// With `-ml 1 -sow` every transcription entry is a single word.
type whisperJSON struct {
	Transcription []struct {
		Text    string `json:"text"`
		Offsets struct {
			From int64 `json:"from"` // milliseconds
			To   int64 `json:"to"`
		} `json:"offsets"`
	} `json:"transcription"`
}

// runWhisper transcribes a 16 kHz mono WAV into word stamps.
func runWhisper(ctx context.Context, t *Tools, wavPath string) ([]WordStamp, error) {
	outPrefix := strings.TrimSuffix(wavPath, filepath.Ext(wavPath)) + "_whisper"
	// English-only models (ggml-*.en.bin) reject -l auto; multilingual ones
	// want it. Decide from the model filename, which whisper.cpp's own
	// naming convention makes reliable.
	lang := "auto"
	base := strings.ToLower(filepath.Base(t.WhisperModel))
	// English-only model names: ggml-base.en.bin, ggml-base.en-q5_1.bin, …
	if strings.Contains(base, ".en.") || strings.Contains(base, ".en-") ||
		strings.HasSuffix(base, ".en.bin") {
		lang = "en"
	}
	args := []string{
		"-m", t.WhisperModel,
		"-f", wavPath,
		"-l", lang,
		"-ml", "1", "-sow", // one word per segment → explicit word timestamps
		"-oj", "-of", outPrefix,
		"-np", // no prints beyond results; keeps stderr readable on error
	}
	cmd := exec.CommandContext(ctx, t.Whisper, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr // -np keeps this quiet; capture anyway for errors
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("whisper failed: %s", firstLine(stderr.String()))
	}
	// Size-capped read (defense-in-depth): with the pinned binary the JSON
	// is bounded by audio length, but a 64 MB ceiling costs nothing.
	f, err := os.Open(outPrefix + ".json")
	if err != nil {
		return nil, fmt.Errorf("whisper produced no output JSON: %v", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, 64<<20))
	f.Close()
	_ = os.Remove(outPrefix + ".json")
	if err != nil {
		return nil, fmt.Errorf("reading whisper output: %v", err)
	}
	var wj whisperJSON
	if err := json.Unmarshal(data, &wj); err != nil {
		return nil, fmt.Errorf("whisper output JSON invalid: %v", err)
	}
	words := make([]WordStamp, 0, len(wj.Transcription))
	for _, seg := range wj.Transcription {
		text := strings.TrimSpace(seg.Text)
		if text == "" || seg.Offsets.To <= seg.Offsets.From {
			continue
		}
		words = append(words, WordStamp{
			Text:  text,
			Start: float64(seg.Offsets.From) / 1000,
			End:   float64(seg.Offsets.To) / 1000,
		})
	}
	return words, nil
}

// ---------- sherpa-onnx diarization ----------

// diarLineRe matches sherpa-onnx-offline-speaker-diarization stdout lines:
//
//	0.318 -- 6.865 speaker_00
var diarLineRe = regexp.MustCompile(`^\s*([0-9]+(?:\.[0-9]+)?)\s*--\s*([0-9]+(?:\.[0-9]+)?)\s+speaker_([0-9]+)\s*$`)

// runDiarization returns who-spoke-when segments for a 16 kHz mono WAV.
// speakersExpected > 0 pins the cluster count (the UI's "speakers expected"
// hint); 0 lets the clustering threshold decide.
func runDiarization(ctx context.Context, t *Tools, wavPath string, speakersExpected int) ([]SpeakerSegment, error) {
	args := []string{
		"--segmentation.pyannote-model=" + t.DiarSegModel,
		"--embedding.model=" + t.DiarEmbModel,
	}
	if speakersExpected > 0 {
		args = append(args, fmt.Sprintf("--clustering.num-clusters=%d", speakersExpected))
	}
	args = append(args, wavPath)
	cmd := exec.CommandContext(ctx, t.Diarizer, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("diarization failed: %s", firstLine(stderr.String()))
	}
	var segs []SpeakerSegment
	for _, line := range strings.Split(stdout.String(), "\n") {
		m := diarLineRe.FindStringSubmatch(line)
		if m == nil {
			continue // config echo, "Started", timing lines
		}
		start, err1 := strconv.ParseFloat(m[1], 64)
		end, err2 := strconv.ParseFloat(m[2], 64)
		spk, err3 := strconv.Atoi(m[3])
		if err1 != nil || err2 != nil || err3 != nil || end <= start {
			continue
		}
		segs = append(segs, SpeakerSegment{Start: start, End: end, Speaker: spk})
	}
	sort.Slice(segs, func(i, k int) bool { return segs[i].Start < segs[k].Start })
	return segs, nil
}

// speakerLetter renders diarization cluster N as the UI's speaker label
// ("A", "B", … "Z", then "AA"… for the absurd case).
func speakerLetter(n int) string {
	if n < 0 {
		n = 0
	}
	s := ""
	for {
		s = string(rune('A'+n%26)) + s
		n = n/26 - 1
		if n < 0 {
			return s
		}
	}
}

// assignSpeakers labels each word with the diarization segment its midpoint
// falls in; words outside every segment take the nearest segment (by gap
// distance). With no segments at all — diarization found no speech even
// though whisper did — every word is "A", which degrades to a plain
// single-speaker transcript instead of failing the job.
func assignSpeakers(words []WordStamp, segs []SpeakerSegment) []spokenWord {
	out := make([]spokenWord, len(words))
	for i, w := range words {
		out[i].WordStamp = w
		if len(segs) == 0 {
			out[i].Speaker = "A"
			continue
		}
		mid := (w.Start + w.End) / 2
		best, bestDist := 0, -1.0
		for k, s := range segs {
			var d float64
			switch {
			case mid >= s.Start && mid < s.End:
				d = 0
			case mid < s.Start:
				d = s.Start - mid
			default:
				d = mid - s.End
			}
			if bestDist < 0 || d < bestDist {
				best, bestDist = k, d
			}
			if d == 0 {
				break
			}
		}
		out[i].Speaker = speakerLetter(segs[best].Speaker)
	}
	return out
}

// chunksFromWords groups speaker-labelled words into dialogue chunks: split
// on speaker change, on a ≥0.6 s inter-word gap, or at 14 words. Identical
// grouping rules to the v1.1–v1.3 fallback path, now the primary (and only)
// chunker — deterministic given the same word/segment inputs.
func chunksFromWords(words []spokenWord) []TranscriptChunk {
	const maxWords = 14
	const gapSec = 0.6
	var out []TranscriptChunk
	var cur *TranscriptChunk
	flush := func() {
		if cur != nil && len(cur.Words) > 0 {
			texts := make([]string, len(cur.Words))
			for i, w := range cur.Words {
				texts[i] = w.Text
			}
			cur.Text = strings.Join(texts, " ")
			cur.WordCount = len(cur.Words)
			cur.End = cur.Words[len(cur.Words)-1].End
			out = append(out, *cur)
		}
		cur = nil
	}
	for _, w := range words {
		if cur == nil {
			cur = &TranscriptChunk{Speaker: w.Speaker, Start: w.Start}
		} else if len(cur.Words) >= maxWords ||
			(len(cur.Words) > 0 && w.Start-cur.Words[len(cur.Words)-1].End >= gapSec) ||
			(w.Speaker != "" && w.Speaker != cur.Speaker) {
			flush()
			cur = &TranscriptChunk{Speaker: w.Speaker, Start: w.Start}
		}
		cur.Words = append(cur.Words, w.WordStamp)
	}
	flush()
	return out
}
