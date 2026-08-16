# Architecture

```
┌─────────────────────────── browser ────────────────────────────┐
│ frontend/index.html  (single file; served at / or from file://)│
│                                                                │
│  <video> preview ── canvas overlay (live caption preview)      │
│  video ──► POST /api/transcribe ──► dialogue chunks OR         │
│            (no audio track) scene-cut chunks ──► cue list      │
│  cue text ──► canvas render ──► one transparent PNG per cue    │
│  settings + video + cue PNGs ──► POST /api/jobs                │
│  poll GET /api/jobs/{id} ──► download /api/jobs/{id}/result    │
└────────────────────────────────────────────────────────────────┘
                              │ 127.0.0.1 only · CORS "*" (no credentials)
┌─────────────────────────── server ─────────────────────────────┐
│ Go stdlib only.  main.go / server.go / jobs.go / pipeline.go   │
│                                                                │
│  /api/transcribe → whisper.cpp + sherpa-onnx diarization, or   │
│                    scenedetect.go's scdet fallback if no audio │
│  /api/jobs      → stream parts to per-job 0700 dir             │
│                   → ffprobe validate → JobSpec.Validate        │
│                   → job queue (1 encode at a time)             │
│                                                                │
│  pipeline:  ffmpeg -ss/-to -i video -i cue_*.png               │
│             -filter_complex_script:                            │
│               [0:v] fps → scale (lanczos, no dither)           │
│                     → pad (bar style) → format=rgba [base]     │
│               [k:v] scale → overlay enable='between(t,S,E)'    │
│               … chained per cue … → format=yuv420p [out]       │
│                                                                │
│   encoder=gifski : [out] → Y4M pipe → gifski → out.gif         │
│   encoder=ffmpeg : pass1 palettegen → pass2 paletteuse(dither) │
└────────────────────────────────────────────────────────────────┘
```

## The caption mechanism ("change when the video changes")

Captions are **timestamp-driven**: each cue becomes one full-canvas
transparent PNG rendered by the *browser* (WYSIWYG — the preview and the
output share the drawing code) and composited by FFmpeg's `overlay` filter
gated with `enable='between(t,start,end)'`. At each cue boundary the active
overlay switches — that is the meme-style caption change. Overlapping cues are
both drawn (parser warns).

Cue times arrive as OUTPUT-timeline seconds (time in the assembled, trimmed
GIF — the v1.3 contract; with N clips "source time" is ambiguous, so the
frontend maps per-clip source times to output times and frame-quantizes the
clip offsets). `Validate` clamps to `[0, TotalDuration]` and drops
out-of-window cues. Legacy v1.2-shaped requests (no `clips[]` array) still
carry source-absolute times; the server rebases those via
`rebaseLegacyCues` for compatibility.

## Multi-clip (v1.3)

1..8 clips, each trimmed independently (`-ss`/`-to` per input), each scaled
per-input (`fps → scale → letterbox pad → format=rgba → setsar=1`) and
joined with `concat=n=N:v=1:a=0` into one continuous timeline before the
bar-pad/overlay tail. **Clip 0 defines the canvas** (its aspect at the
requested width); later clips letterbox into it, centered on black, computed
as integers in Go (`fitEven`) so the graph never contains arithmetic.
Per-input fps before concat keeps the assembled timeline uniform so the
half-open overlay enable windows align with output time exactly.

Transcription is **one whisper+diarization run per clip, sequentially** —
an owner design constraint: diarization labels are clip-scoped ("Speaker A"
in clip 2 is unrelated to clip 1's "A"), so a new clip can never surface as
a confusing "Speaker 4" continuation. Chunks carry a `clip` index with
clip-local times; the UI badges "Clip N · Speaker X" and the import step
maps chunk times through each clip's current trim window onto the assembled
timeline.

Why not server-side text? Rendering text on the server would drag in a font
raster stack (libass had 2 fresh CVEs fixed only in 0.17.5; drawtext needs
fontconfig/freetype and its escaping rules are a classic injection footgun).
Browser-rendered PNGs remove that entire surface and make the preview honest.

## Determinism

Two different guarantees, stated separately on purpose (v1.4):

**The encode path** (everything between "Convert" and the finished GIF):

- No ML, no RNG, no time-dependent inputs anywhere in it.
- FFmpeg path: `palettegen` (Heckbert median-cut) and every `paletteuse`
  dither are deterministic algorithms; `sws_dither=none` keeps scaling exact.
- gifski path: verified byte-identical across runs in the test suite.
- The test suite SHA-256-compares two full pipeline runs for both encoders.
- Caveat (documented, not hidden): determinism is guaranteed per
  binary-version. A different FFmpeg/gifski build may produce different
  (equally valid) bytes. `bin/TOOL-VERSIONS.txt` records what you ran.

**The transcribe path** (whisper.cpp + sherpa-onnx — the one ML feature):

- Reproducible **run-to-run on the same machine with the same model files
  and binary versions** — verified by double-run comparison in
  `TestRealDiarizationIntegration`.
- NOT bit-exact across different hardware, SIMD paths, or thread counts —
  no neural inference stack is, and we don't pretend otherwise. This is
  why the product claim is scoped to the encode path: transcription output
  feeds a human editing step (you rewrite the script), so cross-machine
  bit-equality was never load-bearing there.
- `bin/TOOL-VERSIONS.txt` records model SHA-256s alongside binary versions,
  so "what produced this" is always answerable.

## Geometry contract (client ↔ server)

Both sides compute the same canvas: `vidH = even(round(W·srcH/srcW))`
(mirrors `scale=W:-2` rounding), canvas = `vidH + barPx` for bar style. The
server additionally rescales every cue PNG to the canvas before overlay, so a
±1 px client rounding difference can never misalign or fail the encode.

## Job lifecycle

`queued → encoding → done | error | cancelled`. One encode at a time
(semaphore); progress parsed from `-progress` key=value output on stderr
(no extra goroutines — an incremental line parser implements io.Writer).
Job dirs: `os.TempDir()/gifsmith/run-<pid>/<32-hex-id>/`, `0700`, deleted on
cancel, swept after 2 h, removed on shutdown. IDs come from crypto/rand;
the ID regexp (`^[a-f0-9]{32}$`) is the only path component ever used.

## API

| method & path | body | returns |
|---|---|---|
| `GET /` | — | the UI |
| `GET /fonts/{name}` | — | allow-listed OFL font files (if fetched) |
| `GET /api/config` | — | versions, warn threshold, platform table, limits |
| `POST /api/jobs` | multipart `settings` JSON + `video_0…video_N` (≤8; `video` = clip 0) + `cue_N` PNGs | `202 {id, estimateBytes}` |
| `GET /api/jobs/{id}` | — | state/progress/size |
| `DELETE /api/jobs/{id}` | — | cancel + cleanup |
| `GET /api/jobs/{id}/result` | — | `image/gif` attachment |
| `POST /api/transcribe` | multipart `settings` JSON (`speakersExpected?`) + `video_0…video_N` (≤8) | `202 {id, state, clipCount}` |
| `GET /api/transcribe/{id}` | — | state (`extracting/transcribing/diarizing/done/error`) + `clipCount/clipsDone` + `chunks[]` (clip-tagged, clip-local times) |
| `DELETE /api/transcribe/{id}` | — | cancel + cleanup |
| `GET /api/transcribe/{id}/thumb?clip=K&t=S` | — | `image/jpeg` scene frame from clip K at clip-local S (done jobs only) |
| `GET /api/debug/log` (`?format=json`) | — | diagnostic trace: env header + ring buffer (2000 events) |

## Transcribe feature (fully local since v1.4)

Per clip, sequentially: `ffmpeg -vn -ac 1 -ar 16000 -c:a pcm_s16le →
audio.wav` → `whisper-cli -ml 1 -sow -oj` (one word per JSON segment →
explicit word timestamps; `-l auto` for multilingual models, `-l en` for
`.en` ones) → `sherpa-onnx-offline-speaker-diarization` (pyannote-
segmentation-3.0 + TitaNet embeddings; `speakersExpected` maps to
`--clustering.num-clusters`) → each word takes the speaker segment its
midpoint falls in (nearest segment if none; single-speaker "A" if the
diarizer finds no speech at all) → a deterministic grouper (split at
≥0.6 s gaps / 14 words / speaker change — the same rules since v1.1)
becomes dialogue chunks (clip index, speaker letter, start/end seconds,
text, per-word timestamps, word count). The browser renders each chunk as:
scene thumbnail (server-extracted at the chunk midpoint, cached, atomic
tmp+rename) above the transcribed words row and a blank script row;
"Import from Transcribe" on the Convert tab turns chunks into caption cues
(user script where filled, original dialogue otherwise) with a visible
success badge.

**There is no network-egress path in the product** — v1.1–v1.3's AssemblyAI
upload (and its API-key plumbing, per-request key field, EU-endpoint flag,
and hourly credit-burn rate limit) was removed entirely in v1.4. The tools
are separate pinned subprocesses, same posture as ffmpeg/gifski; if
they're missing, the Transcribe endpoints 503 and everything else works.
Caps: 2 concurrently *processing* jobs (excess sit in a `queued` state; the
wall-clock timeout starts when processing starts), 8 concurrent upload
reservations, 40 retained jobs, **30 clips/hour** (the CPU-burn guard —
same role the credit-burn guard played in v1.1–v1.3, since any local page
can reach the API), 300 thumbnails/job/clip, 2 concurrent thumbnail
extractions, 20-minute-per-clip timeout (capped at 90 min/job).

Settings JSON: `trimStart trimEnd width fps style barPx barPos barColor
encoder quality dither maxColors cues[{start,end}]` — every field clamped or
rejected server-side in `JobSpec.Validate` regardless of what the client sent.
