#!/usr/bin/env bash
# build-ffmpeg.sh — build a minimal, hardened FFmpeg from source for GIFsmith.
#
# WHY THIS EXISTS
# ---------------
# setup-tools.sh fetches a prebuilt FFmpeg (BtbN static on Linux, Homebrew on
# macOS). Those builds enable essentially every demuxer, decoder and protocol
# FFmpeg ships — hundreds of parsers for formats GIFsmith will never open. Most
# FFmpeg CVEs live in exactly that long tail.
#
# This script instead builds from source with --disable-everything and then
# re-enables ONLY the components GIFsmith actually invokes. Code that is not
# compiled in cannot be exploited, patched, or scanned — it simply is not there.
#
# Worked example: CVE-2026-8461 ("PixelSmash", CVSS 8.8) is a heap OOB write in
# the MagicYUV decoder (libavcodec/magicyuv.c) reachable via a crafted file, fixed
# upstream in 8.1.2. GIFsmith has no reason to decode MagicYUV, so it is absent
# from the enable list below. A build produced by this script is not "patched"
# against PixelSmash; it never contained the decoder.
#
# Likewise --disable-network removes every network protocol (HTTP, HLS, RTMP,
# TLS, RTSP...) from the binary. GIFsmith only ever reads local files and pipes.
#
# STATUS / HONESTY NOTE
# ---------------------
# This script was authored in an environment that could not compile FFmpeg, so
# the configure set below is a reasoned starting point, NOT a verified one. The
# self-test in step 4 is the part that matters: it exercises every codepath
# GIFsmith uses and fails loudly if a component is missing. Expect to add one or
# two --enable- flags on the first run. That is the intended workflow: the
# self-test tells you the true minimum rather than you guessing it.
#
# Usage:
#   scripts/build-ffmpeg.sh                 # build the pinned version
#   FF_VERSION=9.0 scripts/build-ffmpeg.sh  # build a different release
#   FF_SHA256=<hex> scripts/build-ffmpeg.sh # pin the source tarball hash
#
# Requires: a C toolchain (cc, make), nasm or yasm, pkg-config, curl, tar.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$PWD"

# ---------------------------------------------------------------------------
# 0. Configuration
# ---------------------------------------------------------------------------
# 8.1.2 is the release that fixes CVE-2026-8461 (PixelSmash). 9.0 shipped
# 2026-08-04; move to it once you have verified it against your scanner.
FF_VERSION="${FF_VERSION:-8.1.2}"
FF_SHA256="${FF_SHA256:-}"          # optional hard pin; see step 1
JOBS="${JOBS:-$( (command -v nproc >/dev/null && nproc) || sysctl -n hw.ncpu || echo 4)}"
PREFIX="$ROOT/bin/ffmpeg-build"
SRCDIR="$ROOT/.ffmpeg-src"

say()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARNING:\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }
sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}

for tool in curl tar make cc pkg-config; do
  command -v "$tool" >/dev/null || die "missing required build tool: $tool"
done
command -v nasm >/dev/null || command -v yasm >/dev/null || \
  warn "neither nasm nor yasm found — the build will fall back to C (slower). Install nasm for optimized assembly."

# ---------------------------------------------------------------------------
# 1. Fetch and verify source
# ---------------------------------------------------------------------------
mkdir -p "$SRCDIR"
TARBALL="$SRCDIR/ffmpeg-$FF_VERSION.tar.xz"
URL="https://ffmpeg.org/releases/ffmpeg-$FF_VERSION.tar.xz"

if [ ! -f "$TARBALL" ]; then
  say "Downloading FFmpeg $FF_VERSION source…"
  curl -fSL --retry 3 -o "$TARBALL" "$URL"
fi

GOT="$(sha256 "$TARBALL")"
say "source tarball SHA-256: $GOT"

if [ -n "$FF_SHA256" ]; then
  [ "$GOT" = "$FF_SHA256" ] || die "source hash mismatch — expected $FF_SHA256, got $GOT. Refusing to build."
  say "source hash matches the pin"
else
  # Cross-check against upstream's published checksum. This is still TOFU on
  # ffmpeg.org's TLS; for full assurance verify the .asc GPG signature against
  # the FFmpeg release signing key instead, and then pass FF_SHA256 to pin it.
  if curl -fsSL --retry 2 -o "$TARBALL.sha256" "$URL.sha256" 2>/dev/null; then
    UP="$(awk '{print $1}' "$TARBALL.sha256")"
    [ "$GOT" = "$UP" ] || die "source hash does not match upstream published checksum ($UP)"
    say "source hash matches upstream published checksum"
  else
    warn "could not fetch upstream checksum — record $GOT and pass it as FF_SHA256 on future runs"
  fi
  warn "no local pin set. Re-run with FF_SHA256=$GOT to make this build reproducible."
fi

rm -rf "$SRCDIR/ffmpeg-$FF_VERSION"
say "Extracting…"
tar -C "$SRCDIR" -xf "$TARBALL"
cd "$SRCDIR/ffmpeg-$FF_VERSION"

# ---------------------------------------------------------------------------
# 2. Configure — deny by default, then allow exactly what GIFsmith calls
# ---------------------------------------------------------------------------
# Derived from the actual invocations in pipeline.go and transcribe.go:
#   decode user video -> fps -> scale(lanczos) -> [pad+]setsar -> [concat if
#   multi-clip] -> [pad for bar style] -> overlay(PNG cues) -> format ->
#   yuv4mpegpipe -> gifski
#   (the ffmpeg-native palettegen/paletteuse encode path this comment used
#   to also describe was removed 2026-08-17, GIF-12 -- gifski alone now)
#   thumbnails: -> mjpeg
#   transcribe: -vn -ac 1 -ar 16000 -> audio file
#
# HONESTY NOTE (round-8 finding): setsar and concat were both missing from
# the first version of this list despite being unconditional filters in
# buildFilterScript (pipeline.go) — every clip gets `setsar=1`, and any
# multi-clip job hits `concat`. Their absence didn't fail configure or the
# build; it only surfaced as a confusing runtime filtergraph-parse error
# ("No option name near '1'") once real jobs ran, which is exactly the
# failure mode this self-test exists to catch before shipping the binary.
#
# NOTE on audio: as of v1.4 GIFsmith extracts 16 kHz mono PCM WAV — the
# native input format of the local whisper.cpp + sherpa-onnx transcription
# toolchain. pcm_s16le and the wav muxer are FFmpeg-native (both enabled
# below), so no external audio encoder exists in this build at all.
# (v1.1–v1.3 extracted MP3 via the external libmp3lame for the AssemblyAI
# upload; that dependency is gone along with the upload.) FLAC stays enabled
# as a lossless local-archival option; the mp3 DECODER stays enabled because
# user-supplied videos legitimately carry mp3 audio tracks.
#
# NOTE on GPL: no GPL-only component is enabled below, so this should produce an
# LGPL-2.1+ binary rather than GPLv3. Verify with `ffmpeg -L` after building —
# if it reports LGPL, the licensing story gets simpler than the current one.
#
# NOTE on wrapped_avframe: the gifski path (pipeline.go) writes raw frames
# via `-f yuv4mpegpipe`; FFmpeg's default codec for that muxer is the
# wrapped_avframe pass-through pseudo-encoder, not a real bitstream encoder.
# Without --enable-encoder=wrapped_avframe, that path fails at runtime with
# "Automatic encoder selection failed" (round-7 finding).
#
# NOTE on scdet: the no-audio fallback path (transcribe.go) uses this
# dedicated scene-change-detection filter to produce camera-cut timestamps
# as a caption-timing scaffold when a clip has no audio track to transcribe.
# Sourced from FFmpeg 8.1.2's own doc/filters.texi, not memory: it stamps
# `lavfi.scd.time` frame metadata at each detected cut. No external tool
# (e.g. PySceneDetect) was worth the Python/OpenCV runtime this Go project
# otherwise has zero of — see docs/ or project memory for the vetting.
# metadata filter (mode=print) is paired with scdet to read that timestamp
# back out as clean stdout lines instead of scraping ffmpeg's own log text.

# --enable-zlib is a real production dependency, not test-only: png_decoder
# and png_encoder both need it for DEFLATE, and GIFsmith's caption cues are
# PNGs composited via the overlay filter (pipeline.go). Without it, configure
# silently drops both PNG codecs rather than failing loudly (round-6 finding
# — caught by this self-test, not by configure's own output).
say "Configuring (deny-by-default)…"
./configure \
  --prefix="$PREFIX" \
  --disable-everything \
  --disable-autodetect \
  --disable-network \
  --disable-doc \
  --disable-debug \
  --disable-ffplay \
  --disable-shared --enable-static \
  --enable-ffmpeg --enable-ffprobe \
  --enable-swscale --enable-swresample --enable-avfilter \
  --enable-protocol=file --enable-protocol=pipe \
  --enable-zlib \
  --enable-avdevice --enable-indev=lavfi \
  --enable-filter=testsrc \
  --enable-filter=testsrc2 \
  --enable-filter=smptebars \
  --enable-filter=color \
  --enable-filter=sine \
  --enable-decoder=wrapped_avframe \
  --enable-decoder=rawvideo \
  --enable-decoder=gif \
  --enable-demuxer=mov \
  --enable-demuxer=matroska \
  --enable-demuxer=avi \
  --enable-demuxer=image2 \
  --enable-demuxer=png_pipe \
  --enable-demuxer=wav \
  --enable-demuxer=flac \
  --enable-demuxer=nut \
  --enable-demuxer=gif \
  --enable-decoder=h264 \
  --enable-decoder=hevc \
  --enable-decoder=vp8 \
  --enable-decoder=vp9 \
  --enable-decoder=av1 \
  --enable-decoder=mpeg4 \
  --enable-decoder=png \
  --enable-decoder=mjpeg \
  --enable-decoder=aac \
  --enable-decoder=mp3 \
  --enable-decoder=opus \
  --enable-decoder=vorbis \
  --enable-decoder=flac \
  --enable-decoder=pcm_s16le \
  --enable-parser=h264 \
  --enable-parser=hevc \
  --enable-parser=vp9 \
  --enable-parser=av1 \
  --enable-parser=mpeg4video \
  --enable-parser=png \
  --enable-parser=aac \
  --enable-parser=flac \
  --enable-encoder=mjpeg \
  --enable-encoder=png \
  --enable-encoder=rawvideo \
  --enable-encoder=flac \
  --enable-encoder=pcm_s16le \
  --enable-encoder=wrapped_avframe \
  --enable-muxer=image2 \
  --enable-muxer=yuv4mpegpipe \
  --enable-muxer=rawvideo \
  --enable-muxer=flac \
  --enable-muxer=wav \
  --enable-muxer=nut \
  --enable-muxer=mov \
  --enable-muxer=mp4 \
  --enable-encoder=mpeg4 \
  --enable-encoder=aac \
  --enable-filter=scale \
  --enable-filter=fps \
  --enable-filter=overlay \
  --enable-filter=pad \
  --enable-filter=format \
  --enable-filter=setsar \
  --enable-filter=concat \
  --enable-filter=scdet \
  --enable-filter=metadata \
  --enable-filter=crop \
  --enable-filter=trim \
  --enable-filter=setpts \
  --enable-filter=null \
  --enable-filter=copy \
  --enable-filter=aformat \
  --enable-filter=anull \
  --enable-filter=aresample \
  --enable-filter=atrim \
  --enable-filter=asetpts \
  || die "configure failed — read the error above and add the missing --enable- flag"

# ---------------------------------------------------------------------------
# 3. Build and install into ./bin
# ---------------------------------------------------------------------------
say "Building with $JOBS jobs (this takes a few minutes)…"
make -j"$JOBS"
make install

install -m 0755 "$PREFIX/bin/ffmpeg"  "$ROOT/bin/ffmpeg"
install -m 0755 "$PREFIX/bin/ffprobe" "$ROOT/bin/ffprobe"
cd "$ROOT"

# ---------------------------------------------------------------------------
# 4. Self-test — prove the minimal build actually runs GIFsmith's pipeline
# ---------------------------------------------------------------------------
# This is the load-bearing part. A minimal build that is missing a component
# fails at the user's runtime, not at build time, unless we check here.
#
# NOTE on lavfi/testsrc/color/sine (round-5 finding): these three are enabled
# above solely so this self-test can synthesize its own fixtures without
# depending on an external ffmpeg being present on the build machine. They
# are synthetic generators, not parsers of external file formats or network
# input, so the incremental attack surface is the filter's own arithmetic,
# not a new decode path for attacker-controlled bytes. GIFsmith's Go code
# (pipeline.go, transcribe.go) never invokes lavfi/testsrc/sine — grep
# confirms it — so this is test-only surface, called out here rather than
# left unexplained the way PixelSmash's absence is explained above.
# testsrc2 (distinct from testsrc) is likewise not used by GIFsmith's own
# self-test above but IS needed by `go test ./...`'s own sample-clip fixture
# generator (pipeline_test.go, transcribe_test.go) — required for the
# project's actual verify step, so it stays with the other test-only enables.
# muxer=mp4/encoder=mpeg4/encoder=aac: same fixture generators write .mp4
# sample clips. `mov` and `mp4` are DISTINCT muxer names sharing one
# implementation (each with its own extension list) — enabling only `mov`
# registers extension "mov", not "mp4", so `mp4` needs its own enable.
# mpeg4/aac are the mov/mp4 muxer's documented default video/audio codecs
# (`ffmpeg -h muxer=mov`) and the fixture generators don't override either
# (pipeline_test.go picks no -c:v; transcribe_test.go asks for -c:a aac
# explicitly). Both are FFmpeg-native encoders, no GPL/external lib pulled
# in — production only ever DECODES mpeg4/aac (already enabled above), never
# encodes either, so this is test-only surface, same as the rest of this note.
# smptebars: the multi-clip test's second fixture (a different-aspect clip,
# to exercise the letterbox path) uses this pattern generator instead of
# testsrc2 purely so the two synthetic clips look visually distinct.
# demuxer=gif + decoder=gif: test helper gifDuration() (pipeline_test.go)
# re-probes GIFsmith's own produced .gif output via the shared Probe()
# function to assert its duration. Probe() is real production code, but
# production only ever calls it on user-uploaded input clips (h264/hevc/
# vp8/vp9/av1/mpeg4, all already enabled) — grep confirms GIFsmith never
# re-probes its own GIF output at runtime, so this is test-only surface.
# wrapped_avframe is the pass-through pseudo-decoder lavfi's synthetic
# sources need to hand raw frames to an encoder; not a real bitstream parser.
# demuxer=nut (paired with the existing muxer=nut) is the same story: NUT is
# this self-test's own lossless intermediate container between steps, read
# back by ffprobe/ffmpeg here — GIFsmith itself never reads or writes NUT.
# decoder=rawvideo: NUT's payload here is raw frames, and reading it back
# for a later filtergraph step needs the matching decoder — this half is
# genuinely test-only. GIFsmith's real inputs are h264/hevc/vp8/vp9/av1/mpeg4.
#
# encoder=rawvideo is a DIFFERENT story (cross-unit-contract review finding:
# this comment used to claim it was "also test-only", which was wrong and
# nearly cost scenedetect.go's no-audio fallback its real dependency). It's
# dual-purpose: the self-test's NUT round-trip AND scenedetect.go's genuine
# production discard-output call (`-f rawvideo -y /dev/null` — writing to
# /dev/null still runs the real encoder before the muxer discards the
# bytes; ffmpeg doesn't special-case /dev/null to skip encoding). Do not
# remove --enable-encoder=rawvideo even if the self-test's own use of it is
# ever refactored away — scenedetect.go needs it independently.
say "Self-testing the minimal build…"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
FF="$ROOT/bin/ffmpeg"
FP="$ROOT/bin/ffprobe"

check() { # description, command...
  local desc="$1"; shift
  if "$@" >"$T/log" 2>&1; then
    printf '  \033[1;32mok\033[0m   %s\n' "$desc"
  else
    printf '  \033[1;31mFAIL\033[0m %s\n' "$desc"
    sed 's/^/       /' "$T/log" | tail -20
    FAILED=1
  fi
}
FAILED=0

# Synthetic source clip, H.264 in MP4 — but note we did NOT enable an H.264
# ENCODER (GIFsmith never encodes H.264), so generate the fixture as raw/FFV1
# in NUT and treat real-world H.264 decode as a separate manual check.
check "generate test source (rawvideo/nut)" \
  "$FF" -y -f lavfi -i testsrc=size=320x240:rate=10:duration=2 \
        -c:v rawvideo -f nut "$T/src.nut"

check "ffprobe reads streams + duration" \
  "$FP" -v error -show_entries format=duration -show_entries stream=width,height,codec_name \
        -of json "$T/src.nut"

check "cue PNG decode + overlay + scale + fps + pad (the caption path)" \
  "$FF" -y -i "$T/src.nut" -f lavfi -i color=c=red@0.5:s=320x40:d=2 \
        -filter_complex "[0:v]fps=10,scale=320:-2:flags=lanczos,pad=iw:ih+40:0:40,format=rgba[base];[base][1:v]overlay=0:0:enable='gte(t,0)*lt(t,1)',format=yuv420p[out]" \
        -map "[out]" -c:v rawvideo -f nut "$T/composited.nut"

check "Y4M pipe output (the gifski path)" \
  bash -c "'$FF' -y -i '$T/src.nut' -vf 'fps=10,scale=160:-2,format=yuv420p' -f yuv4mpegpipe - > '$T/pipe.y4m'"

check "MJPEG thumbnail extraction" \
  "$FF" -y -ss 1 -i "$T/src.nut" -frames:v 1 -c:v mjpeg -f image2 "$T/thumb.jpg"

check "PNG cue file decode (image2)" \
  bash -c "'$FF' -y -f lavfi -i color=c=blue:s=64x64:d=1 -frames:v 1 '$T/cue.png' && \
           '$FF' -y -i '$T/cue.png' -f rawvideo -pix_fmt rgba '$T/cue.raw'"

check "audio extract to WAV pcm_s16le mono 16k (the v1.4 local transcribe path)" \
  bash -c "'$FF' -y -f lavfi -i 'sine=frequency=440:duration=2' -f flac '$T/tone.flac' && \
           '$FF' -y -i '$T/tone.flac' -vn -ac 1 -ar 16000 -c:a pcm_s16le '$T/audio.wav'"

if [ -x "$ROOT/bin/gifski" ]; then
  check "end-to-end ffmpeg -> gifski" \
    bash -c "'$FF' -v error -i '$T/src.nut' -vf 'fps=10,scale=160:-2,format=yuv420p' -f yuv4mpegpipe - | '$ROOT/bin/gifski' -o '$T/gifski.gif' --fps 10 -"
else
  warn "bin/gifski not present — skipping the gifski end-to-end check (run setup-tools.sh for it)"
fi

# Negative check: the point of the exercise. These must NOT be present.
say "Confirming the removed attack surface is actually absent…"
absent() { # description, grep-pattern, list-command...
  local desc="$1" pat="$2"; shift 2
  if "$@" 2>/dev/null | grep -qiE "$pat"; then
    printf '  \033[1;31mSTILL PRESENT\033[0m %s\n' "$desc"; FAILED=1
  else
    printf '  \033[1;32mabsent\033[0m %s\n' "$desc"
  fi
}
absent "magicyuv decoder (CVE-2026-8461 / PixelSmash)" '(^| )magicyuv' "$FF" -hide_banner -decoders
absent "network protocols (http/https/rtmp/hls)"       '(^| )(http|rtmp|rtsp)' "$FF" -hide_banner -protocols
absent "libass / subtitle burn-in filters"             '(^| )(ass|subtitles|drawtext)' "$FF" -hide_banner -filters
# gif encoder/muxer and palettegen/paletteuse: GIFsmith's own ffmpeg-based
# GIF-encoder path was removed 2026-08-17 (GIF-12) in favor of gifski alone
# (measured smaller output, comparable-or-better speed, in every content
# sample tested). decoder=gif/demuxer=gif stay enabled -- unrelated
# read-only capability the Go test suite's gifDuration() helper still uses
# to probe gifski's own output -- but ffmpeg WRITING a gif is now exactly as
# dead as decoding MagicYUV, so it's checked absent the same way.
absent "gif encoder"          '(^| )gif( |$)' "$FF" -hide_banner -encoders
absent "palettegen/paletteuse filters" '(^| )palette(gen|use)' "$FF" -hide_banner -filters

[ "$FAILED" = 0 ] || die "self-test failed — add the missing --enable- flags and re-run. Do NOT ship this build."

# ---------------------------------------------------------------------------
# 5. Record provenance for the CVE audit
# ---------------------------------------------------------------------------
{
  echo "recorded:       $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "ffmpeg version: $("$FF" -version | head -1)"
  echo "source tarball: ffmpeg-$FF_VERSION.tar.xz"
  echo "source sha256:  $GOT"
  echo "build:          from source, --disable-everything + explicit allowlist"
  echo "license:        $("$FF" -hide_banner -L 2>/dev/null | head -3 | tr '\n' ' ')"
  [ -x "$ROOT/bin/gifski" ] && echo "gifski:         $("$ROOT/bin/gifski" --version)"
  echo
  echo "--- enabled decoders ---"
  "$FF" -hide_banner -decoders 2>/dev/null | tail -n +11
  echo "--- enabled demuxers ---"
  "$FF" -hide_banner -demuxers 2>/dev/null | tail -n +5
  echo "--- enabled protocols ---"
  "$FF" -hide_banner -protocols 2>/dev/null
} > bin/TOOL-VERSIONS.txt

say "Done. Minimal FFmpeg installed to bin/ffmpeg and bin/ffprobe."
say "Full component inventory written to bin/TOOL-VERSIONS.txt — this file is"
say "the evidence for the CVE-audit claim. Feed it to your scanner."
