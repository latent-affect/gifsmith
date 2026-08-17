package main

// estimate.go — pre-encode size estimation and the platform autoplay table.
//
// The warn threshold default (30 MB) is the mean of the GIF-autoplay upload
// limits verified from official platform documentation on 2026-08-05:
// X 15 + Discord 10 + Tumblr 10 + GIPHY 100 + Mastodon 16 = 151 / 5 ≈ 30.2.
// General file-sharing caps (Slack 1 GB, Telegram 2 GB, Signal 100 MB) are
// listed in the table for reference but excluded from the mean because they
// are not GIF-autoplay limits. Sources in docs/domain-briefing.md.

// Platform is one row of the reference table shown in the UI.
type Platform struct {
	Name    string  `json:"name"`
	LimitMB float64 `json:"limitMB"`
	// InMean marks platforms whose limit is a GIF-autoplay limit (used for
	// the default warn threshold) rather than a general file cap.
	InMean bool   `json:"inMean"`
	Note   string `json:"note"`
}

// Platforms verified 2026-08-05; see docs/domain-briefing.md for source URLs.
var Platforms = []Platform{
	{"X (Twitter)", 15, true, "API cap; served as MP4"},
	{"Discord (free)", 10, true, "general 10 MB file cap applies to GIFs"},
	{"Tumblr", 10, true, "≤3 MB served untouched; ≤10 MB compressed"},
	{"GIPHY", 100, true, "upload cap; ≤8 MB recommended"},
	{"Mastodon", 16, true, "server default; converted to MP4"},
	{"Slack", 1024, false, "general file cap, not GIF-specific"},
	{"Telegram", 2048, false, "general cap; GIFs become MP4"},
	{"Signal", 100, false, "attachment cap"},
}

// DefaultWarnMB is the mean of the InMean platforms above.
const DefaultWarnMB = 30.2

// EstimateBytes predicts output size before encoding. GIF size is strongly
// content-dependent, so this is an order-of-magnitude heuristic, clearly
// labeled as such in the UI. Basis: ~0.5 MB/s at 480×270 @15fps observed in
// practitioner benchmarks (docs/domain-briefing.md §3) ⇒ ≈0.26 bytes per
// pixel per frame at gifski's default quality (90); this is the ONLY
// encoder path since 2026-08-17 (GIF-12 removed the ffmpeg palettegen/
// paletteuse alternative). qualityMultiplier mirrors the frontend's own
// curve (frontend/index.html, currentBpp/qualityMultiplier) — kept as two
// independent implementations of the SAME calibration data rather than one
// shared source, because this Go copy and the JS copy have no natural
// shared module; if the curve is ever recalibrated, both need updating
// together (see frontend/index.html's comment for the measurement this
// curve is based on).
func EstimateBytes(width, height int, fps, seconds float64, quality int) int64 {
	bpp := 0.30 * qualityMultiplier(quality)
	frames := fps * seconds
	px := float64(width * height)
	return int64(px * frames * bpp)
}

func qualityMultiplier(q int) float64 {
	if q < 1 {
		q = 1
	} else if q > 100 {
		q = 100
	}
	points := [][2]float64{{1, 0.20}, {10, 0.24}, {50, 0.41}, {90, 1.0}, {100, 1.29}}
	qf := float64(q)
	for i := 1; i < len(points); i++ {
		qa, ma := points[i-1][0], points[i-1][1]
		qb, mb := points[i][0], points[i][1]
		if qf <= qb {
			return ma + (mb-ma)*(qf-qa)/(qb-qa)
		}
	}
	return points[len(points)-1][1]
}
