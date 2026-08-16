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
// pixel per frame; error-diffusion dither and high quality raise it.
func EstimateBytes(width, height int, fps, seconds float64, encoder, dither string) int64 {
	bpp := 0.26
	switch {
	case encoder == "gifski":
		bpp = 0.30 // per-frame palettes keep more detail ⇒ slightly larger
	case dither == "none":
		bpp = 0.18
	case dither == "bayer":
		bpp = 0.22
	}
	frames := fps * seconds
	px := float64(width * height)
	return int64(px * frames * bpp)
}
