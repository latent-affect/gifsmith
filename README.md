# GIFsmith

Convert `.mp4` / `.mov` video into GIFs with **meme-style captions driven by a
subtitle file** — the captions change exactly when the subtitle timestamps
change. Runs **entirely on your machine** — including transcription: a small
Go server (loopback only), a single-file HTML frontend, and zero network
egress. Nothing you feed it goes anywhere.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for how the pieces fit together.

## Features

- **Subtitle-timed captions** — upload an `.srt` or `.vtt`; each cue becomes a
  caption overlay burned into the GIF for exactly its time window. The parser
  tolerates real-world mess: BOMs, CRLF, Windows-1252/UTF-16 encodings, broken
  indices, comma *or* dot milliseconds, embedded HTML tags.
- **Multi-clip (v1.3)** — up to 8 clips, each trimmed independently, stitched
  in order into one GIF. Clip 1 sets the canvas; different-aspect clips
  letterbox to fit. Captions anchor to their clip and follow trims/reorders.
  Transcription runs **one whisper+diarization pass per clip**, so speaker
  labels stay scoped to their clip ("Clip 2 · Speaker A") instead of
  bleeding across clips.
- **Fully local transcription (v1.4)** — the Transcribe tab runs
  [whisper.cpp] (speech→text, word timestamps) and [sherpa-onnx]
  (who-spoke-when diarization) as pinned local binaries. **Your audio never
  leaves your machine** — no API, no account, no key, no HuggingFace token.
  Binaries and diarization models are fetched once by
  `scripts/setup-tools.sh --transcribe` with hard SHA-256 pins; the whisper
  model is verified against upstream's published hash (hard-pinnable via
  `WHISPER_MODEL_SHA256`). Then everything runs offline.
- **Two caption styles, WYSIWYG** — classic meme (white Anton/Impact with black
  outline, top or bottom, over the video) or a caption bar (white/black bar
  added above or below the frame). The browser renders the exact pixels that
  get composited, so the preview cannot lie. Cue text is editable in the UI.
- **Any size** — output width 16–3840 px (aspect preserved). FPS capped at 50
  because browsers clamp GIF frame delays below 2 centiseconds to 100 ms — a
  faster GIF would actually play *slower*.
- **No hard duration cap, honest size warnings instead** — a warning fires when
  the estimated or actual output exceeds the **30 MB** default threshold: the
  mean of the GIF-autoplay upload limits verified from official platform docs
  on 2026-08-05 (X 15, Discord 10, Tumblr 10, GIPHY 100, Mastodon 16). A
  per-platform pass/fail table is shown after every encode. Threshold is
  configurable (`-warn-mb`).
- **Two encoders** — [gifski] (highest quality; per-frame palettes) and FFmpeg
  `palettegen`/`paletteuse` (fast, smaller files, dither selectable). The
  **encode path is byte-deterministic**, verified in this repo's test suite:
  same input + same settings ⇒ identical bytes, no ML anywhere in it. (The
  Transcribe tab is the one ML feature — reproducible run-to-run on the same
  machine+model, but neural inference is not bit-exact across hardware, so
  we don't claim it is. See docs/ARCHITECTURE.md §Determinism.)
- **Minimal attack/audit surface** — the server is Go **standard library
  only** (zero third-party Go dependencies). Media binaries are pinned and
  checksum-verified by `scripts/setup-tools.sh`. Full audit:
  [docs/CVE-AUDIT.md](docs/CVE-AUDIT.md).

## Quick start

```sh
# 1. Fetch the pinned media binaries (ffmpeg, ffprobe, gifski) into ./bin
scripts/setup-tools.sh

# 2. (Optional but recommended) fetch the OFL meme fonts into ./fonts
scripts/fetch-fonts.sh

# 3. Build & run the server (Go ≥1.24; zero third-party packages)
go build -o gifsmith-server .
./gifsmith-server            # → http://127.0.0.1:8437/

# 4. Open the UI
#    either  http://127.0.0.1:8437/        (served by the binary)
#    or      frontend/index.html           (double-click; same file, CORS-ready)
```

Windows: download the same archives manually (`win/gifski.exe` from the gifski
1.34.0 release; a BtbN `win64-gpl` FFmpeg build), drop `ffmpeg.exe`,
`ffprobe.exe`, `gifski.exe` into `bin\`, then `go build` as above.

Server flags: `-port` (default 8437), `-warn-mb` (default 30.2), `-max-upload-mb`
(default 2048), `-tools` (binaries dir), `-fonts` (fetched-fonts dir, defaults
to `fonts/` next to the binary — same convention as `-tools`), `-tmp` (work
dir), `-whisper-model` (override the transcription model file). The server binds to
**127.0.0.1 only** — it is not reachable from the network, and makes no
outbound connections either.

## Transcribe tab — dialogue → meme script (fully local since v1.4)

Upload a clip (or reuse the Convert tab's), and GIFsmith extracts the audio
track and transcribes + diarizes it **on your machine**: whisper.cpp
produces word-level timestamps, sherpa-onnx (pyannote-segmentation-3.0 +
NeMo TitaNet embeddings) labels who spoke when, and a deterministic grouper
turns that into dialogue chunks — a scene thumbnail above the transcribed
words (with word count and clip-scoped speaker badge/timestamps), and a
blank row where you write your own script for that moment.

The transcribed text is a **timing scaffold, not a subtitle product**: it
shows you where each line lands so you can write your own caption there.
That design choice is why local whisper accuracy is enough — you're going
to replace the words anyway, and the timestamps are what matter. On the
Convert tab, **Import from Transcribe** pulls the chunks in as captions —
your script where you wrote one, the original line where you didn't — with
a green badge confirming what was imported. After converting you get the
actual GIF file size plus the per-platform autoplay verdicts, and the
**Resolution** presets / **Auto-fit ≤ warn** button pick a width that keeps
longer clips under the size threshold.

Setup: `scripts/setup-tools.sh --transcribe` (builds whisper.cpp at a
pinned commit, fetches the sherpa-onnx binary and all model files with
SHA-256 verification — about 200 MB once, then fully offline). Model size
is `WHISPER_MODEL=base` by default; `small`/`medium` trade speed for
accuracy. **Privacy: nothing leaves your machine — there is no network
code in the transcribe path at all.** (v1.1–v1.3 used the AssemblyAI API;
that entire egress path was removed in v1.4.)

## Using it (Convert)

1. Choose a video. Trim with the ⇤/⇥ buttons if you want a section.
2. Choose a subtitle file. Cues appear in a list — edit text inline, untick
   cues you don't want, click a timestamp to preview that moment.
3. Pick style (classic/bar), position, font, size, outline.
4. Pick width/fps/encoder, watch the size estimate and platform chips.
5. Convert → progress bar → download. The result reports actual size against
   the autoplay threshold and every platform's limit.

## Debugging

If anything goes wrong, hit **Export debug log** (bottom of the page). It
downloads one text file combining the client-side event log (UI actions,
JS errors, unhandled rejections) with the server's diagnostic ring buffer
(request trace, encode/transcribe lifecycles with timings and tool-error
excerpts, environment/versions header). Error banners point at it. The
trace contains **no secrets by construction** — and since v1.4 there are
no API keys anywhere in the product to leak in the first place.

## Tests

```sh
go test ./...        # unit + integration (needs ./bin binaries; ~10 s)
go test -short ./... # unit tests only
```

The integration suite generates a deterministic test clip, runs both encoder
paths twice and asserts byte-identical output, verifies caption-bar geometry
with ffprobe, drives the full HTTP API exactly as the frontend does, and
probes traversal/malformed-ID handling.

## Repository map

| path | what |
|---|---|
| `main.go` `server.go` `jobs.go` `pipeline.go` `estimate.go` | Go server (stdlib only) |
| `subtitle/` | tolerant SRT/VTT parser + table tests |
| `frontend/index.html` | the whole UI — single file, no build step, no JS deps |
| `site/index.html` | the public marketing page — single file, self-contained, offline |
| `scripts/` | pinned toolchain fetcher, font fetcher |
| `docs/` | architecture, CVE audit, sourced domain briefing (platform limits, size-estimate constants) |
| `testdata/` | sample subtitle file |

## Security posture (summary — full audit in docs/CVE-AUDIT.md)

- Loopback bind; permissive CORS is safe because there are no credentials and
  no state a cross-origin page could abuse that isn't already local.
- No shell anywhere: `exec` argv arrays; user filenames never touch paths;
  jobs live in per-job `0700` temp dirs keyed by crypto-random IDs.
- Filter graphs go through `-filter_complex_script` files — a 1000-cue
  subtitle can't overflow a command line or inject filter syntax.
- Upload caps, per-part caps, PNG magic-byte checks, ffprobe validation
  before any decode.

## License

MIT for this repository's code. FFmpeg static builds are GPLv3 (separate
binaries, fetched by script, not linked). gifski is AGPL-3.0 (separate binary,
invoked as a subprocess). whisper.cpp is MIT (built from pinned source);
sherpa-onnx is Apache-2.0 (separate binary, subprocess); model files:
pyannote-segmentation-3.0 is MIT, NeMo TitaNet-small is CC-BY-4.0 (both
fetched with attribution recorded in `bin/TOOL-VERSIONS.txt`), whisper ggml
models are MIT (OpenAI Whisper weights as converted by whisper.cpp). Anton &
Oswald fonts are SIL OFL 1.1 (fetched with their license texts).

[gifski]: https://gif.ski/
[whisper.cpp]: https://github.com/ggml-org/whisper.cpp
[sherpa-onnx]: https://github.com/k2-fsa/sherpa-onnx
