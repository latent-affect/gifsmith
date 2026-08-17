#!/usr/bin/env bash
# setup-tools.sh — download the pinned media binaries into ./bin.
#
# Policy (docs/CVE-AUDIT.md):
#   * gifski 1.34.0 — version-pinned immutable URL, hard SHA-256 verify.
#     (Zero CVEs on record for gifski as of 2026-08-05.)
#   * FFmpeg — built from source by scripts/build-ffmpeg.sh, NOT fetched as a
#     prebuilt binary. A prebuilt (BtbN static, Homebrew, apt) enables
#     essentially every demuxer/decoder/protocol FFmpeg ships; this project's
#     whole CVE-attack-surface story (docs/CVE-AUDIT.md) depends on running
#     the --disable-everything + explicit-allowlist build instead. This
#     script used to fetch a prebuilt FFmpeg directly (BtbN on Linux, brew on
#     macOS) as a SECOND, undeclared producer of bin/ffmpeg alongside
#     build-ffmpeg.sh — whichever ran last silently won, and re-running this
#     script after build-ffmpeg.sh had already produced a hardened binary
#     replaced it with a full-featured one with no warning (confirmed
#     2026-08-16, FORE-14). Fixed by removing the second producer: this
#     script now calls build-ffmpeg.sh directly, so there is exactly one
#     path that ever writes bin/ffmpeg.
#
# Local transcription toolchain (v1.4, optional — pass --transcribe):
#   * whisper.cpp v1.9.2 — built from source at a pinned commit (MIT). The
#     ggml model file is a plain HTTPS file download from Hugging Face's CDN
#     (no account, no token, no HF tooling), verified against the SHA-256
#     that upstream publishes in the repo's LFS pointer. HONEST CAVEAT: the
#     pointer and the payload come from the same host, so this is transfer-
#     integrity verification, not an independent pin — set
#     WHISPER_MODEL_SHA256 for a hard pin (the installed model's hash is
#     recorded in bin/TOOL-VERSIONS.txt for exactly that purpose).
#   * sherpa-onnx v1.13.2 — prebuilt static diarization binary, hard
#     SHA-256 pin (Apache-2.0), plus two ONNX models from k2-fsa's GitHub
#     releases, hard SHA-256 pins: pyannote-segmentation-3.0 (MIT) and
#     NeMo TitaNet-small English speaker embeddings (CC-BY-4.0).
#   Everything runs locally; nothing in the transcribe path touches the
#   network after this script finishes.
#
# Usage: scripts/setup-tools.sh [--transcribe]   (run from the repo root)
set -euo pipefail

WITH_TRANSCRIBE=0
for a in "$@"; do
  case "$a" in
    --transcribe) WITH_TRANSCRIBE=1 ;;
    *) echo "unknown flag: $a (supported: --transcribe)"; exit 2 ;;
  esac
done

cd "$(dirname "$0")/.."
mkdir -p bin

GIFSKI_URL="https://github.com/ImageOptim/gifski/releases/download/1.34.0/gifski-1.34.0.tar.xz"
GIFSKI_SHA256="b9b6591aa163123d737353d9c8581efdf3234d28eeaa45329b31da905cd5a996"

OS="$(uname -s)"
ARCH="$(uname -m)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

say()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARNING:\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | awk '{print $1}';
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}

fetch() { # url out
  curl -fSL --retry 3 -o "$2" "$1"
}

# ---------------- gifski (all platforms from the one release archive) ----
say "Fetching gifski 1.34.0 (SHA-256 pinned)…"
fetch "$GIFSKI_URL" "$TMP/gifski.tar.xz"
GOT="$(sha256 "$TMP/gifski.tar.xz")"
[ "$GOT" = "$GIFSKI_SHA256" ] || die "gifski archive SHA-256 mismatch (got $GOT) — refusing to install"
case "$OS" in
  Linux)  GIFSKI_MEMBER="linux/gifski" ;;
  Darwin) GIFSKI_MEMBER="mac/gifski" ;;
  *)      GIFSKI_MEMBER="" ; warn "unsupported OS for gifski auto-install ($OS); see README for Windows (win/gifski.exe is in the same archive)";;
esac
if [ -n "$GIFSKI_MEMBER" ]; then
  tar -C "$TMP" -xf "$TMP/gifski.tar.xz" "$GIFSKI_MEMBER" 2>/dev/null \
    || tar -C "$TMP" -xf "$TMP/gifski.tar.xz" "./$GIFSKI_MEMBER"
  install -m 0755 "$TMP/$GIFSKI_MEMBER" bin/gifski
  say "gifski installed: $(bin/gifski --version)"
fi

# ---------------- ffmpeg (built from source, hardened -- see policy above) ----
case "$OS" in
  Linux|Darwin) : ;;
  *) die "unsupported OS: $OS — see README for manual installation" ;;
esac

say "Building FFmpeg from source (--disable-everything + explicit allowlist)…"
say "This is the ONLY producer of bin/ffmpeg -- see scripts/build-ffmpeg.sh for why."
bash scripts/build-ffmpeg.sh
say "ffmpeg installed: $(bin/ffmpeg -version | head -1)"

# ---------------- local transcription toolchain (optional) --------------
# Two subprocess binaries + three model files. Same posture as ffmpeg/gifski:
# pinned versions, hard hashes wherever the artifact is immutable, and a
# self-check before declaring success. After this section completes, the
# transcribe feature runs with zero network access.

WHISPER_TAG="v1.9.2"
WHISPER_COMMIT="306c88f4d1286aec1bf96e544632897886af5501"
# Model size: base = multilingual, ~148 MB, good-enough placeholder text
# (the product design assumes the user rewrites the script anyway).
# Override with WHISPER_MODEL=small etc.
WHISPER_MODEL="${WHISPER_MODEL:-base}"

SHERPA_VER="v1.13.2"
SHERPA_LINUX_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/${SHERPA_VER}/sherpa-onnx-${SHERPA_VER}-linux-x64-static.tar.bz2"
SHERPA_LINUX_SHA256="07cb28745880bf1746678bf4036c8c05dd44bf5cd038f5c73f38a14df884da9c"
SHERPA_OSX_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/${SHERPA_VER}/sherpa-onnx-${SHERPA_VER}-osx-universal2-static.tar.bz2"
SHERPA_OSX_SHA256="11c56c6577812e30ad3422a826a07f2a4a04b20d926af45b17e7dd9af99dd035"

SEG_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/speaker-segmentation-models/sherpa-onnx-pyannote-segmentation-3-0.tar.bz2"
SEG_SHA256="24615ee884c897d9d2ba09bb4d30da6bb1b15e685065962db5b02e76e4996488"
EMB_URL="https://github.com/k2-fsa/sherpa-onnx/releases/download/speaker-recongition-models/nemo_en_titanet_small.onnx"
EMB_SHA256="ad4a1802485d8b34c722d2a9d04249662f2ece5d28a7a039063ca22f515a789e"

install_transcribe() {
  mkdir -p bin/models

  # -- whisper.cpp: build from source at a pinned commit ------------------
  if [ -x bin/whisper-cli ]; then
    say "whisper-cli already present — skipping build"
  else
    command -v cmake >/dev/null || die "building whisper.cpp needs cmake (apt/brew install cmake)"
    command -v git   >/dev/null || die "building whisper.cpp needs git"
    say "Cloning whisper.cpp ${WHISPER_TAG} (pinned commit ${WHISPER_COMMIT})…"
    git clone -q --depth 1 --branch "$WHISPER_TAG" \
      https://github.com/ggml-org/whisper.cpp.git "$TMP/whisper.cpp"
    GOTC="$(git -C "$TMP/whisper.cpp" rev-parse HEAD)"
    [ "$GOTC" = "$WHISPER_COMMIT" ] || die "whisper.cpp ${WHISPER_TAG} resolves to unexpected commit $GOTC — refusing to build"
    say "Building whisper-cli (this takes a few minutes)…"
    cmake -S "$TMP/whisper.cpp" -B "$TMP/whisper.cpp/build" \
      -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF \
      -DWHISPER_BUILD_TESTS=OFF -DGGML_NATIVE=OFF >/dev/null
    cmake --build "$TMP/whisper.cpp/build" -j"$(nproc 2>/dev/null || sysctl -n hw.ncpu)" \
      --target whisper-cli >/dev/null
    install -m 0755 "$TMP/whisper.cpp/build/bin/whisper-cli" bin/whisper-cli
  fi

  # -- whisper ggml model: plain HTTPS file download, hash-verified -------
  # The model is a static file on Hugging Face's CDN — no account, token,
  # or HF tooling involved. Its SHA-256 is read from the repo's published
  # LFS pointer first, so the payload is verified against what upstream
  # says it should be (and recorded in TOOL-VERSIONS.txt). A hard local
  # pin via WHISPER_MODEL_SHA256 wins over the remote value if set.
  MODEL_FILE="bin/models/ggml-${WHISPER_MODEL}.bin"
  if [ -f "$MODEL_FILE" ]; then
    say "whisper model $(basename "$MODEL_FILE") already present — skipping"
  else
    PTR_URL="https://huggingface.co/ggerganov/whisper.cpp/raw/main/ggml-${WHISPER_MODEL}.bin"
    BIN_URL="https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-${WHISPER_MODEL}.bin"
    WANT="${WHISPER_MODEL_SHA256:-}"
    if [ -z "$WANT" ]; then
      WANT="$(curl -fsSL --retry 3 "$PTR_URL" | awk '/^oid sha256:/ {sub("sha256:","",$2); print $2}')" || true
    fi
    [ -n "$WANT" ] || die "could not determine the published SHA-256 for ggml-${WHISPER_MODEL}.bin (set WHISPER_MODEL_SHA256 to pin manually)"
    say "Fetching whisper model ggml-${WHISPER_MODEL}.bin (SHA-256 ${WANT:0:12}…)…"
    if [ -z "${WHISPER_MODEL_SHA256:-}" ]; then
      warn "model hash was read from upstream's LFS pointer (same host as the payload):"
      warn "this verifies transfer integrity, not upstream compromise. To hard-pin:"
      warn "  WHISPER_MODEL_SHA256=$WANT scripts/setup-tools.sh --transcribe"
    fi
    fetch "$BIN_URL" "$TMP/model.bin"
    GOT="$(sha256 "$TMP/model.bin")"
    [ "$GOT" = "$WANT" ] || die "whisper model SHA-256 mismatch (got $GOT, want $WANT) — refusing to install"
    install -m 0644 "$TMP/model.bin" "$MODEL_FILE"
  fi

  # -- sherpa-onnx diarization binary (prebuilt static, hard pin) ---------
  if [ -x bin/sherpa-onnx-offline-speaker-diarization ]; then
    say "sherpa-onnx diarizer already present — skipping"
  else
    case "$OS" in
      Linux)
        [ "$ARCH" = "x86_64" ] || die "pinned sherpa-onnx binary targets x86_64; on $ARCH build from source (https://github.com/k2-fsa/sherpa-onnx)"
        S_URL="$SHERPA_LINUX_URL"; S_SHA="$SHERPA_LINUX_SHA256" ;;
      Darwin)
        S_URL="$SHERPA_OSX_URL";   S_SHA="$SHERPA_OSX_SHA256" ;;
      *) die "unsupported OS for sherpa-onnx auto-install: $OS" ;;
    esac
    say "Fetching sherpa-onnx ${SHERPA_VER} (SHA-256 pinned)…"
    fetch "$S_URL" "$TMP/sherpa.tar.bz2"
    GOT="$(sha256 "$TMP/sherpa.tar.bz2")"
    [ "$GOT" = "$S_SHA" ] || die "sherpa-onnx archive SHA-256 mismatch (got $GOT) — refusing to install"
    # Full extract, then install the one binary — GNU tar needs --wildcards
    # for member globs while macOS bsdtar rejects that flag, so a selective
    # extract is not portable (review round-4 finding).
    mkdir -p "$TMP/sherpa"
    tar -C "$TMP/sherpa" -xjf "$TMP/sherpa.tar.bz2"
    install -m 0755 "$TMP"/sherpa/sherpa-onnx-*/bin/sherpa-onnx-offline-speaker-diarization \
      bin/sherpa-onnx-offline-speaker-diarization
  fi

  # -- diarization models (hard pins; from k2-fsa GitHub releases) --------
  if [ ! -f bin/models/sherpa-segmentation.onnx ]; then
    say "Fetching pyannote-segmentation-3.0 ONNX (SHA-256 pinned)…"
    fetch "$SEG_URL" "$TMP/seg.tar.bz2"
    GOT="$(sha256 "$TMP/seg.tar.bz2")"
    [ "$GOT" = "$SEG_SHA256" ] || die "segmentation model SHA-256 mismatch (got $GOT) — refusing to install"
    tar -C "$TMP" -xjf "$TMP/seg.tar.bz2"
    install -m 0644 "$TMP/sherpa-onnx-pyannote-segmentation-3-0/model.onnx" bin/models/sherpa-segmentation.onnx
    install -m 0644 "$TMP/sherpa-onnx-pyannote-segmentation-3-0/LICENSE" bin/models/sherpa-segmentation.LICENSE 2>/dev/null || true
  fi
  if [ ! -f bin/models/sherpa-embedding.onnx ]; then
    say "Fetching NeMo TitaNet-small speaker embeddings (SHA-256 pinned)…"
    fetch "$EMB_URL" "$TMP/emb.onnx"
    GOT="$(sha256 "$TMP/emb.onnx")"
    [ "$GOT" = "$EMB_SHA256" ] || die "embedding model SHA-256 mismatch (got $GOT) — refusing to install"
    install -m 0644 "$TMP/emb.onnx" bin/models/sherpa-embedding.onnx
  fi

  # -- self-check: really run both tools before declaring success ---------
  say "Self-check: generating 2s test tone and running both tools…"
  bin/ffmpeg -hide_banner -loglevel error -f lavfi -i "sine=frequency=440:duration=2" \
    -ac 1 -ar 16000 -c:a pcm_s16le -y "$TMP/check.wav"
  bin/whisper-cli -m "$MODEL_FILE" -f "$TMP/check.wav" -ml 1 -sow -oj -of "$TMP/check" -np \
    || die "whisper-cli self-check failed"
  [ -f "$TMP/check.json" ] || die "whisper-cli produced no JSON output"
  bin/sherpa-onnx-offline-speaker-diarization \
    --segmentation.pyannote-model=bin/models/sherpa-segmentation.onnx \
    --embedding.model=bin/models/sherpa-embedding.onnx \
    "$TMP/check.wav" >/dev/null 2>&1 || die "sherpa-onnx diarization self-check failed"
  say "transcribe toolchain OK (a pure tone transcribes to silence — that is expected)"
}

if [ "$WITH_TRANSCRIBE" = 1 ]; then
  install_transcribe
else
  say "Transcribe toolchain skipped (pass --transcribe to enable the fully-local Transcribe tab)"
fi

# Appended (>>), not written fresh (>): build-ffmpeg.sh already wrote its own
# ffmpeg provenance block (version, source hash, full enabled decoder/demuxer/
# protocol enumeration -- the actual CVE-audit evidence) to this same file as
# its own last step. Overwriting that with a thinner one here was the original
# bug shape all over again, just at the log-file level instead of the binary
# level -- don't repeat it.
#
# NOTE: every conditional below is a full if/fi — a bare `[ … ] && cmd` as
# a block's last statement returns 1 under `set -e` when the test fails and
# would abort the script after an otherwise-successful install (round-4
# review finding).
{
  echo
  echo "--- setup-tools.sh additional tools ---"
  echo "recorded: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  if [ -x bin/gifski ]; then
    echo "gifski:  $(bin/gifski --version)"
  fi
  if [ -x bin/whisper-cli ]; then
    echo "whisper-cli: whisper.cpp ${WHISPER_TAG} (commit ${WHISPER_COMMIT}, built from source)"
    for m in bin/models/ggml-*.bin; do
      if [ -f "$m" ]; then
        echo "whisper model: $(basename "$m") sha256=$(sha256 "$m")"
      fi
    done
  fi
  if [ -x bin/sherpa-onnx-offline-speaker-diarization ]; then
    echo "sherpa-onnx: ${SHERPA_VER} (diarization binary)"
    if [ -f bin/models/sherpa-segmentation.onnx ]; then
      echo "diar segmentation: pyannote-segmentation-3.0 sha256=$(sha256 bin/models/sherpa-segmentation.onnx)"
    fi
    if [ -f bin/models/sherpa-embedding.onnx ]; then
      echo "diar embedding: nemo_en_titanet_small sha256=$(sha256 bin/models/sherpa-embedding.onnx)"
    fi
  fi
} >> bin/TOOL-VERSIONS.txt
say "Done. Versions recorded in bin/TOOL-VERSIONS.txt"
