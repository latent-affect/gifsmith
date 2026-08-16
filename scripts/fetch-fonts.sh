#!/usr/bin/env bash
# fetch-fonts.sh — download the optional OFL-licensed meme fonts into ./fonts.
#
# The app works without these: the caption font stack falls back to a locally
# installed Impact / Arial Black. Fetching Anton gives everyone the classic
# meme look regardless of what their OS ships. Both fonts are licensed under
# the SIL Open Font License 1.1, which explicitly permits this kind of
# bundling; the OFL texts are downloaded alongside.
#
# NOTE: checksums are printed but not enforced. This project was built in a
# sandbox whose egress proxy blocked font CDNs, so known-good hashes could
# not be recorded at build time (documented in docs/CVE-AUDIT.md §residual).
# Compare the printed SHA-256 against https://fonts.google.com if you need
# provenance beyond HTTPS+the source repo.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p fonts

say() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

fetch() { # url out
  curl -fSL --retry 3 -o "$2" "$1" || return 1
  printf '    %s  SHA-256: ' "$(basename "$2")"
  if command -v sha256sum >/dev/null; then sha256sum "$2" | awk '{print $1}'; else shasum -a 256 "$2" | awk '{print $1}'; fi
}

say "Anton (SIL OFL 1.1) — the free Impact-alike…"
fetch "https://raw.githubusercontent.com/google/fonts/main/ofl/anton/Anton-Regular.ttf" fonts/Anton-Regular.ttf \
  || fetch "https://cdn.jsdelivr.net/fontsource/fonts/anton@latest/latin-400-normal.ttf" fonts/Anton-Regular.ttf
fetch "https://raw.githubusercontent.com/google/fonts/main/ofl/anton/OFL.txt" fonts/OFL-Anton.txt || true

say "Oswald SemiBold (SIL OFL 1.1) — for the caption-bar style…"
fetch "https://cdn.jsdelivr.net/fontsource/fonts/oswald@latest/latin-600-normal.woff2" fonts/Oswald-SemiBold.woff2 \
  || echo "    (Oswald skipped — optional)"
fetch "https://raw.githubusercontent.com/google/fonts/main/ofl/oswald/OFL.txt" fonts/OFL-Oswald.txt || true

say "Done. Restart the server (or reload the page) to pick the fonts up."
