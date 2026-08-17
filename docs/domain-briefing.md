# Domain briefing — GIF encoding, platform limits, media stack, CVE landscape
*(compiled 2026-08-05 for the GIFsmith build · recheck frontier ≈ 2026-11)*

**Confidence & ceiling:** compiled from primary sources (specs, project docs,
browser source, official platform help pages) via live web search on the date
above, by four parallel research passes. Items that could not be verified are
flagged inline — nothing unverified is load-bearing without a mitigation.
CVE/version facts and platform limits drift fast: re-verify anything here
that you are about to rely on months later.

---

## 1 · GIF format physics (why the product behaves as it does)

- **Palettes:** GIF89a allows a global color table and per-frame local color
  tables, each ≤256 entries. Per-frame local palettes are how gifski gets
  "thousands of colors" into one GIF. One index can be flagged transparent
  (FFmpeg's `palettegen` reserves it by default; we disable that —
  `reserve_transparent=0` — because output is opaque). [W3C GIF89a spec]
- **Timing:** frame delay is a 2-byte integer in **centiseconds**. Only
  100/n fps are exactly representable; 30 fps is not (33.3 fps at 3 cs is).
  **Browsers clamp delays ≤10 ms to 100 ms** (verified in Firefox source
  `image/FrameTimeout.h`; Chrome behavior matches — Mozilla bug 1511298
  closed WONTFIX for Chrome parity). Consequence: **>50 fps GIFs play at
  10 fps**. GIFsmith clamps fps to 50 and says why in the UI.
  [gecko-dev; bugzilla 1511298; biphelps.com 2022]
- **Compression:** LZW only (12-bit codes max), no motion compensation.
  The only temporal tools are disposal methods + drawing sub-rectangles +
  transparent-unchanged-pixel runs. Size therefore grows ~linearly with
  duration; a GIF is routinely 5–10× the equivalent H.264 MP4.
- **Size levers, in order of power:** resolution (quadratic) > duration ≈
  fps (linear) > quality/colors (fractional but real -- measured 2026-08-17
  as a 5.4x swing across gifski's own --quality 1..100 range on real
  content, not merely "fractional" in practice) > dither choice (error-
  diffusion *fights* LZW by injecting noise). Ballpark: ≈0.5 MB/s at 480 px/
  15 fps at gifski's default quality=90 (practitioner figures;
  content-dependent by several ×, confirmed by direct measurement across
  three content samples spanning a 9x range at fixed settings). The
  estimator in `estimate.go` encodes this heuristic PLUS a quality-scaling
  curve (`qualityMultiplier`) calibrated against real gifski output --
  quality used to be accepted and silently ignored by the estimator, fixed
  2026-08-16/17. [cleverutils; svgator; gifski README]
- **FFmpeg quality workflow (historical -- this project's own use of it was
  removed 2026-08-17, GIF-12; kept here as general domain knowledge, not a
  claim about this codebase):** default one-pass conversion uses a generic
  palette (banding); correct is two-pass `palettegen` → `paletteuse`
  (Heckbert median-cut, 1982). `stats_mode=full|diff|single` trades static
  vs moving fidelity. Dithers available (`bayer`, `sierra2_4a` default,
  `floyd_steinberg`, `none`, …) are all deterministic — none use randomness.
  Real head-to-head measurement against gifski (three content samples) found
  this path producing LARGER output than gifski every time (1.4x-4.3x), and
  gifski faster in two of three -- contradicts the "speed, smaller files"
  framing this project's own docs used to carry for it.
  [ubitux (filter author) blog 2015; vf_palettegen.c/vf_paletteuse.c]
- **gifski:** libimagequant (pngquant) engine, per-frame palettes + temporal
  dithering; quality ceiling among GIF encoders; its own README warns GIF is
  a bad codec regardless. Accepts PNG frames or **Y4M on stdin** (what we
  pipe). [gif.ski; ImageOptim/gifski README]

## 2 · Platform GIF-autoplay limits (verified 2026-08-05, official pages)

| platform | limit | source quality | in warn-mean? |
|---|---|---|---|
| X/Twitter | 15 MB | official API docs (help-center pages 404'd; mobile split unverified) | ✅ |
| Discord (free) | 10 MB | official support FAQ (lowered from 25 MB, Sept 2024) | ✅ |
| Tumblr | 10 MB (≤3 MB served untouched) | official help | ✅ |
| GIPHY | 100 MB upload (≤8 MB recommended) | official support | ✅ |
| Mastodon | 16 MB default, per-instance | official docs (GIF→MP4) | ✅ |
| Slack | 1 GB | official help — **general file cap** | ❌ |
| Telegram | 2 GB | official FAQ — general cap; GIFs are MP4s there | ❌ |
| Signal | 100 MB | project GitHub only — weakly verified | ❌ |
| Reddit | n/a | **no authoritative figure exists**; secondary sources conflict (20 vs 100 MB) | excluded |
| Imgur | ~200 MB | third-party mirror only (official pages unreachable) | excluded |

**Warn threshold = mean of the ✅ column = 151/5 ≈ 30.2 MB** (owner's
directive: warn at the mean of what platforms accept for autoplay; the
general file caps would inflate the mean to ~415 MB and aren't autoplay
limits). Implemented as `-warn-mb` default; full table shipped in the UI.

## 3 · Stack & subtitle findings that shaped the design

- **FFmpeg releases:** 9.0 "Lei" released **2026-08-04** (day before audit).
  Static-build providers lag by days; BtbN's n9.0 asset 404'd at build time
  (checked) → setup script's try-9.0-fallback-8.1 design. Native libavcodec
  decodes H.264, HEVC, ProRes — .mov/.mp4 coverage without extra libs.
- **SRT has no spec** — tolerate BOM, CRLF, Windows-1252/UTF-16 encodings
  ("any SubRip parser must attempt charset detection" — Wikipedia/SubRip;
  cdown/srt library docs list the same real-world breakages our parser
  handles). **VTT is W3C**: `WEBVTT` header, dot milliseconds, optional
  hours, NOTE/STYLE/REGION blocks, cue settings after timestamps.
- **subtitles-burning options rejected:** FFmpeg `subtitles` filter needs
  libass (CVE pinning problem — see CVE-AUDIT); `drawtext` needs freetype +
  its escaping rules are an injection footgun. Chosen instead:
  browser-rendered per-cue PNGs + `overlay enable='between(t,…)'` — zero
  server font surface, WYSIWYG by construction.
- **overlay EOF behavior:** a single-frame PNG input ends immediately;
  `overlay`'s default `eof_action=repeat` holds that frame forever, and
  `enable` gates when it's applied — the efficient standard pattern for
  timed still overlays (no `-loop 1` re-decoding).
- **Fonts:** Impact is Microsoft-licensed, **not redistributable**; using a
  locally installed copy to raster output is explicitly fine per Microsoft's
  redistribution FAQ. Free Impact-alike: **Anton** (SIL OFL 1.1, bundleable
  with OFL.txt); Oswald (OFL) for the bar style. Meme conventions: uppercase,
  white fill, black outline ≈7–10 % of font size (practitioner convention,
  no authoritative spec — exposed as a slider). [google/fonts OFL files;
  learn.microsoft.com font-redistribution Q&A]
- **CORS for a file:// frontend:** file:// pages send `Origin: null`; the
  server answers `Access-Control-Allow-Origin: *` (never the literal string
  `null` — MDN/W3C warn it also matches sandboxed iframes). `*` is safe here:
  no credentials, loopback bind. Chrome's Local Network Access rollout
  (v142+, 2025) can gate localhost fetches — mitigation: the server serves
  the identical UI itself, so same-origin use is always available. [MDN;
  developer.chrome.com/blog/local-network-access]

## 4 · CVE landscape

Condensed into [CVE-AUDIT.md](CVE-AUDIT.md), which is the operative document.
Headline facts behind the stack choice: gifski has zero lifetime CVEs
(GH advisories, osv.dev, RustSec all empty); Go stdlib had zero open CVEs at
go1.26.5; FFmpeg carries the June-2026 Depthfirst disclosures with fixes
confirmed only on master/9.0 at audit time; libass was only clean at exactly
0.17.5; Express/Flask/Node/Python all carried transitive or runtime-coupling
caveats that Go stdlib simply doesn't have.

## Open gaps (honest)

- Whether FFmpeg 8.1.x point releases contain the 39210–39218 fixes —
  unverifiable at audit time; mitigated by the 9.0-preferring setup script
  and the loud fallback warning.
- gifski's temporal-dither determinism had no documentation; settled
  *empirically* here (byte-identical double-runs in the test suite, per
  machine/binary).
- Chrome LNA × file://→127.0.0.1 interaction undocumented; mitigated by
  serving the UI from the binary.

## Primary sources

W3C GIF89a spec · FFmpeg vf_palettegen.c / vf_paletteuse.c / filters docs ·
gecko-dev image/FrameTimeout.h · Mozilla bug 1511298 · biphelps.com "The
Fastest GIF Does Not Exist" · blog.pkh.me "High quality GIF with FFmpeg" ·
gif.ski + ImageOptim/gifski README/releases · docs.x.com media best-practices ·
support.discord.com File Attachments FAQ · help.tumblr.com GIF
troubleshooting · support.giphy.com GIF Creation Best Practices ·
docs.joinmastodon.org/user/posting · slack.com/help Add-files ·
telegram.org/faq · en.wikipedia.org/wiki/SubRip · MDN WebVTT / Origin /
Access-Control-Allow-Origin · github.com/cdown/srt · github.com/libass/libass
releases · learn.microsoft.com Impact-font Q&A · openfontlicense.org OFL-FAQ ·
google/fonts OFL texts · developer.chrome.com/blog/local-network-access ·
ffmpeg.org/security.html · NVD/osv.dev/RustSec/GitHub advisories ·
depthfirst.com "21 zero-days in FFmpeg" · go.dev/doc/devel/release ·
endoflife.date (ffmpeg/python/nodejs).
