# GIFsmith

Point it at a video. GIFsmith transcribes the dialogue itself, or — if the
clip has no audio at all — finds the actual scene cuts and gives you a blank
row at each one. Either way you get real, locally-computed timing to write
your captions against, not a timeline you're dragging text boxes around by
hand and not someone else's subtitle file that may or may not line up with
what's actually on screen.

Everything happens on your machine, transcription included. No signup
screen, no cloud queue, no watermark you have to pay to remove. Just a small
Go server that only listens on loopback, a single HTML file for the UI, and
zero network egress. Feed it a video, and nothing about that video goes
anywhere else.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for how the pieces fit
together.

## Features

- **Cue timing comes from the video itself, not an uploaded file.** There's
  no "upload a subtitle file" path in the UI. An arbitrary `.srt` has no way
  to guarantee it actually matches the cuts in *this* encode of *this*
  clip, and captions that drift from what's on screen are exactly the kind
  of confident-looking-but-wrong output this tool exists to not produce —
  so the shipped app never gives you a way to write a caption against
  anything but timing it just derived from the loaded clip. (This is a
  property of the UI, not a server-side lock: `POST /api/jobs` itself takes
  whatever cue timing and images it's handed, same as it always has, and
  cue text has always been yours to edit freely once a cue exists — the
  guarantee is that the *shipped* path to get a cue never runs through
  someone else's file.) The Transcribe tab runs [whisper.cpp] for local
  speech-to-text with word timestamps and [sherpa-onnx] for local
  who-spoke-when diarization, both pinned binaries — your audio never
  leaves your machine, no API, no account, no key. If a clip has no audio
  track at all, there's nothing for whisper to transcribe, so GIFsmith
  falls back to FFmpeg's own scene-cut detection and hands you a blank row
  at every camera cut instead of a dead end — still derived from the actual
  footage, never a file you'd have to trust.
  `scripts/setup-tools.sh --transcribe` fetches the binaries and
  diarization models once, with hard SHA-256 pins (the whisper model is
  checked against upstream's published hash, and you can pin your own via
  `WHISPER_MODEL_SHA256`). After that, it's offline for good.
- **Multi-clip (v1.3).** Stitch up to 8 clips into one GIF, each trimmed on
  its own. Clip 1 sets the canvas; anything with a different aspect ratio
  letterboxes to fit, and captions stay anchored to their clip through trims
  and reorders. Transcription runs one whisper-plus-diarization pass per
  clip, so speaker labels don't wander ("Clip 2 · Speaker A" stays put, it
  doesn't bleed into Clip 3).
- **Two caption styles, and what you see is what you get.** Classic meme
  (white Anton or Impact, black outline, top or bottom) or a caption bar
  above or below the frame. The browser renders the exact pixels that get
  composited into the GIF, so the preview isn't a polite suggestion, it's
  the actual output. Cue text is editable right in the UI.
- **Any size you want.** Output width runs 16 to 3840 px with aspect
  preserved. FPS caps at 50, and that's not us being stingy: browsers clamp
  GIF frame delays under 2 centiseconds to 100 ms, so a "faster" GIF past
  that point actually plays slower. Better you hear it from us than find out
  the hard way.
- **No hard duration cap, just honest warnings.** GIFsmith flags it when the
  estimated or actual output crosses a 30 MB default threshold, the mean of
  the real GIF-autoplay upload limits we verified against official platform
  docs on 2026-08-05 (X 15, Discord 10, Tumblr 10, GIPHY 100, Mastodon 16).
  Every encode ends with a per-platform pass/fail table, so you know exactly
  where your GIF will and won't autoplay. Threshold's configurable with
  `-warn-mb` if you disagree with our math.
- **[gifski] encodes, via real per-frame palettes.** v1.4 shipped a second
  encoder option — FFmpeg's own `palettegen`/`paletteuse` — advertised as
  faster and smaller. Measured head-to-head (2026-08-17, GIF-12) across
  three content types, gifski won on output size every time (1.4x-4.3x
  smaller) with speed a wash to a clear gifski win depending on content —
  the opposite of the original claim. Removed rather than kept as a slower,
  bigger, redundant option; no ML anywhere in the path, and the test suite
  proves gifski's own output is byte-deterministic: same input, same
  settings, identical bytes, every time. (The Transcribe tab is the one ML
  feature in the whole app. It's reproducible run-to-run on the same machine
  and model, but neural inference isn't bit-exact across hardware, so we're
  not going to pretend it is. See docs/ARCHITECTURE.md §Determinism.)
- **A small attack surface, on purpose.** The server is Go standard library
  only, zero third-party Go dependencies. gifski is pinned and
  checksum-verified; FFmpeg is built from source with `--disable-everything`
  plus an explicit allowlist of only the decoders/demuxers/filters GIFsmith
  actually calls (`scripts/build-ffmpeg.sh`, self-tested against the real
  pipeline before it's ever installed) — a prebuilt FFmpeg enables hundreds
  of parsers this app never opens, and most FFmpeg CVEs live in exactly that
  unused long tail. `scripts/setup-tools.sh` runs the build for you as part
  of setup; there is exactly one path that ever produces `bin/ffmpeg`. Full
  writeup in [docs/CVE-AUDIT.md](docs/CVE-AUDIT.md), if you enjoy reading
  about the things that didn't go wrong.
- **The output file doesn't rat you out.** GIF has no EXIF-style container
  to leak into, so most tools are quiet by default, but we checked what
  actually slips through anyway. Turns out gifski hardcodes a small
  "gif.ski" comment tag into every file it writes, with no flag to turn it
  off, so GIFsmith strips that (and any Comment Extension block either
  encoder might ever add) after encoding, verified by hex-dumping real
  output rather than taking anyone's word for it. No file paths, no machine
  name, no software fingerprint. Post it wherever you want; the only thing
  it says about where it came from is wherever you choose to put it.

## Quick start

```sh
# 1. Fetch gifski (pinned + checksummed) and build the hardened FFmpeg from
#    source into ./bin (scripts/build-ffmpeg.sh — see the security posture
#    note above for why this isn't a prebuilt download)
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

Building FFmpeg from source needs a C toolchain: `cc`, `make`, `pkg-config`,
and ideally `nasm` or `yasm` (the build still works without them, just
slower — no optimized assembly). macOS: `xcode-select --install`. Debian/
Ubuntu: `apt install build-essential nasm pkg-config`.

**Windows:** grab the archives by hand instead, `win/gifski.exe` from the
gifski 1.34.0 release and a BtbN `win64-gpl` FFmpeg build. Drop
`ffmpeg.exe`, `ffprobe.exe`, and `gifski.exe` into `bin\`, then `go build`
same as above. (`scripts/build-ffmpeg.sh` is Unix-only; Windows keeps the
prebuilt-binary path, so its FFmpeg carries the full upstream feature set —
see docs/CVE-AUDIT.md before shipping a Windows build in a
security-sensitive context.)

Server flags: `-port` (default 8437), `-warn-mb` (default 30.2),
`-max-upload-mb` (default 2048), `-tools` (binaries dir), `-fonts`
(fetched-fonts dir, defaults to `fonts/` next to the binary, same
convention as `-tools`), `-tmp` (work dir), `-whisper-model` (override the
transcription model file). The server binds to **127.0.0.1 only**: not
reachable from the network, and it doesn't make outbound connections
either.

## Transcribe tab: dialogue to meme script (fully local since v1.4)

Upload a clip (or reuse the one from the Convert tab), and GIFsmith pulls
the audio track and transcribes plus diarizes it on your machine.
whisper.cpp produces word-level timestamps, sherpa-onnx
(pyannote-segmentation-3.0 plus NeMo TitaNet embeddings) figures out who
spoke when, and a deterministic grouper turns all of that into dialogue
chunks: a scene thumbnail above the transcribed words, a word count and
clip-scoped speaker badge with timestamps, and a blank row underneath where
you write your own line for that moment.

Silent clips get the same treatment instead of a rejection. When there's no
audio track, GIFsmith runs FFmpeg's `scdet` filter to find the actual
camera-cut timestamps in the footage and builds one blank row per cut, so
you still get a timing scaffold positioned where something actually
happens on screen, not just evenly-spaced guesses. Write your line, same as
you would against a transcribed one.

That transcribed text (or those scene cuts) is a timing scaffold, not a
subtitle product. It just shows you where each line lands so you know
where to put your own caption. Local whisper accuracy is enough for that
job, since you're going to replace the words anyway; the timestamps are the
part that matters. On the Convert tab, **Import from Transcribe** pulls the
chunks straight into the cue list, no file in between: your script wherever
you wrote one, the original transcribed line wherever you didn't, with a
green badge confirming what got imported. From there it's editable inline,
previewable per cue, burned in on convert — the only path captions take
into a GIF. After converting, you get the actual GIF file size, the
per-platform autoplay verdicts, and a **Resolution** preset or **Auto-fit ≤
warn** button that picks a width to keep longer clips under the size
threshold.

Setup is `scripts/setup-tools.sh --transcribe`. It builds whisper.cpp at a
pinned commit and fetches the sherpa-onnx binary plus every model file with
SHA-256 verification, about 200 MB once, and then it's offline for good.
Model size defaults to `WHISPER_MODEL=base`; `small` and `medium` trade
speed for accuracy. Nothing leaves your machine doing any of this: there's
no network code anywhere in the transcribe path. (v1.1 through v1.3 used
the AssemblyAI API. That entire egress path is gone as of v1.4.)

## Using it (Convert)

1. Choose a video. Trim with the ⇤/⇥ buttons if you want a section.
2. Head to the Transcribe tab, run it, then **Import from Transcribe**.
   Cues appear in a list: edit text inline, untick cues you don't want,
   click a timestamp to preview that moment.
3. Pick a style (classic or bar), position, font, size, outline.
4. Pick width, fps, and quality, and watch the size estimate and platform
   chips update.
5. Hit Convert. Progress bar, then download. The result reports actual size
   against the autoplay threshold and every platform's limit.

## Debugging

If something breaks, hit **Export debug log** at the bottom of the page. It
bundles the client-side event log (UI actions, JS errors, unhandled
rejections) with the server's diagnostic ring buffer (request trace, encode
and transcribe lifecycles with timings, tool-error excerpts, an environment
and versions header) into one text file. Error banners link straight to it.
The trace has no secrets in it by construction, and since v1.4 there aren't
any API keys left in the product to leak in the first place.

## Tests

```sh
go test ./...        # unit + integration (needs ./bin binaries; ~10 s)
go test -short ./... # unit tests only
```

The integration suite makes a deterministic test clip, runs the gifski
encode path twice and logs whether the output was byte-identical (not
hard-asserted — gifski is a threaded encoder), verifies caption-bar
geometry with ffprobe, drives the full HTTP API the same way the frontend
does, and pokes at path traversal and malformed-ID handling to make sure
nothing gives.

## Repository map

| path | what |
|---|---|
| `main.go` `server.go` `jobs.go` `pipeline.go` `estimate.go` `debuglog.go` | Go server core (stdlib only) |
| `transcribe.go` `localasr.go` `scenedetect.go` | Transcribe tab: whisper.cpp + sherpa-onnx diarization, and the scene-cut fallback for silent clips -- the only two caption sources |
| `gifmeta.go` | strips gifski's GIF Comment Extension metadata (the "gif.ski" fingerprint) from encoder output |
| `frontend/index.html` | the whole UI, single file, no build step, no JS deps |
| `scripts/` | pinned toolchain fetcher, font fetcher |
| `docs/` | architecture, CVE audit, sourced domain briefing (platform limits, size-estimate constants) |
| `testdata/` | sample multi-speaker audio for diarization tests |
| `audible-deletion-flag/` | retired code, kept not deleted (the subtitle-file-upload parser + its tests, removed in favor of transcription/scene-cut-only captioning) |

## Security posture (short version; full audit in docs/CVE-AUDIT.md)

- Loopback bind. Permissive CORS is safe here because there are no
  credentials and no local state a cross-origin page could abuse that it
  doesn't already have access to.
- No shell, anywhere: `exec` calls use argv arrays, user filenames never
  touch a path directly, and jobs live in per-job `0700` temp dirs keyed by
  crypto-random IDs.
- Filter graphs go through `-filter_complex_script` files, so a 1000-cue
  caption list can't overflow a command line or sneak filter syntax in
  sideways.
- Upload caps, per-part caps, PNG magic-byte checks, and ffprobe validation,
  all before anything gets decoded.
- Output GIFs are scrubbed of Comment Extension metadata after encoding
  (gifski otherwise embeds a "gif.ski" tag with no way to disable it); GIF
  has no EXIF-equivalent container to begin with.

## License

MIT for this repository's code. FFmpeg static builds are GPLv3 (separate
binaries, fetched by script, not linked). gifski is AGPL-3.0 (separate
binary, invoked as a subprocess). whisper.cpp is MIT (built from pinned
source); sherpa-onnx is Apache-2.0 (separate binary, subprocess); model
files: pyannote-segmentation-3.0 is MIT, NeMo TitaNet-small is CC-BY-4.0
(both fetched with attribution recorded in `bin/TOOL-VERSIONS.txt`), whisper
ggml models are MIT (OpenAI Whisper weights as converted by whisper.cpp).
Anton & Oswald fonts are SIL OFL 1.1 (fetched with their license texts).

[gifski]: https://gif.ski/
[whisper.cpp]: https://github.com/ggml-org/whisper.cpp
[sherpa-onnx]: https://github.com/k2-fsa/sherpa-onnx
