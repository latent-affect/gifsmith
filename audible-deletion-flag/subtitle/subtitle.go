//go:build ignore

// Moved here, not deleted, when subtitle-file upload was removed (the
// operator's own convention for flagged-for-deletion files). Excluded from
// the build via this tag -- retained Go source under a non-package
// directory still gets type-checked by `go build ./...`/`go vet ./...`
// otherwise, which would break the build for a file nothing imports.

// Package subtitle implements a tolerant parser for SRT (SubRip) and WebVTT
// subtitle files.
//
// Design notes (see docs/domain-briefing.md for sources):
//   - SRT has no official spec. Real-world files carry BOMs, CRLF line
//     endings, broken/duplicated indices, dot-vs-comma millisecond
//     separators, and embedded HTML-ish tags. This parser tolerates all of
//     those and records a warning rather than failing where recovery is safe.
//   - WebVTT is W3C-specified: required "WEBVTT" header (BOM permitted),
//     dot millisecond separator, optional hours, NOTE/STYLE/REGION blocks,
//     and cue-setting text after the timestamp line (which we ignore: meme
//     caption placement is chosen in the UI, not taken from the file).
//   - Everything here is deterministic string processing. No ML, no
//     heuristic language detection: input bytes fully determine output.
package subtitle

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Cue is a single subtitle event.
type Cue struct {
	Index int     `json:"index"` // 1-based position after parsing (not the file's counter)
	Start float64 `json:"start"` // seconds
	End   float64 `json:"end"`   // seconds
	Text  string  `json:"text"`  // tag-stripped text; lines joined with "\n"
}

// Result is a parsed subtitle file plus non-fatal warnings.
type Result struct {
	Format   string   `json:"format"` // "srt" or "vtt"
	Cues     []Cue    `json:"cues"`
	Warnings []string `json:"warnings"`
}

// Limits protecting the server from pathological files.
const (
	MaxCues        = 1000
	MaxCueChars    = 500
	MaxFileBytes   = 2 << 20 // 2 MiB is enormous for a subtitle file
	MaxCueDuration = 600.0   // seconds; single cue longer than this is suspect but allowed
)

var (
	// 00:01:02,345 / 00:01:02.345 / 01:02.345 (VTT short form) with 1-3 ms digits.
	timeRe = regexp.MustCompile(`(?:(\d{1,3}):)?(\d{1,2}):(\d{1,2})[.,](\d{1,3})`)
	// Timing line: two timestamps joined by an arrow (SRT "-->" with flexible spacing).
	arrowRe = regexp.MustCompile(`^\s*((?:\d{1,3}:)?\d{1,2}:\d{1,2}[.,]\d{1,3})\s*-{1,3}>\s*((?:\d{1,3}:)?\d{1,2}:\d{1,2}[.,]\d{1,3})(.*)$`)
	// HTML-ish and VTT tags: <i>, </b>, <font color="...">, <c.classname>, <v Speaker>, <00:01:02.000>.
	tagRe = regexp.MustCompile(`<[^>\n]*>`)
	// SRT positioning extension sometimes appended after the timing line: X1:.. Y1:.. etc.
	srtPosRe = regexp.MustCompile(`^\s*X1:-?\d+\s+X2:-?\d+\s+Y1:-?\d+\s+Y2:-?\d+\s*$`)
)

// Parse auto-detects SRT vs WebVTT and parses tolerantly.
// It fails only when the input cannot plausibly be a subtitle file at all.
func Parse(data []byte) (*Result, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("subtitle file is empty")
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("subtitle file too large (%d bytes; max %d)", len(data), MaxFileBytes)
	}

	res := &Result{}

	text, encWarn := decodeToUTF8(data)
	if encWarn != "" {
		res.Warnings = append(res.Warnings, encWarn)
	}

	// Normalize line endings; strip BOM if still present after decode.
	text = strings.TrimPrefix(text, "\uFEFF")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")

	isVTT := false
	start := 0
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "WEBVTT") {
			isVTT = true
			start = i + 1
			// Header block may continue until a blank line (metadata like "Kind: captions").
			for start < len(lines) && strings.TrimSpace(lines[start]) != "" {
				start++
			}
		}
		break
	}
	if isVTT {
		res.Format = "vtt"
	} else {
		res.Format = "srt"
	}

	cues, warns := parseBlocks(lines[start:], isVTT)
	res.Warnings = append(res.Warnings, warns...)

	if len(cues) == 0 {
		return nil, fmt.Errorf("no subtitle cues found (parsed as %s); is this a subtitle file?", strings.ToUpper(res.Format))
	}
	// Deterministic order by start time (stable: preserves file order for
	// ties) BEFORE truncation, so an out-of-order file keeps its temporally
	// earliest cues rather than its first-in-file cues.
	sort.SliceStable(cues, func(i, j int) bool { return cues[i].Start < cues[j].Start })
	if len(cues) > MaxCues {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("file has %d cues; only the earliest %d are used", len(cues), MaxCues))
		cues = cues[:MaxCues]
	}
	for i := range cues {
		cues[i].Index = i + 1
	}

	// Overlap detection (legal in VTT, undefined in SRT; both are rendered stacked).
	for i := 1; i < len(cues); i++ {
		if cues[i].Start < cues[i-1].End {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"cues %d and %d overlap in time; both captions will be shown", i, i+1))
		}
	}

	res.Cues = cues
	return res, nil
}

// parseBlocks walks the file as blank-line-separated blocks, tolerating
// missing counters, stray blank lines inside malformed files, and VTT
// NOTE/STYLE/REGION blocks.
func parseBlocks(lines []string, isVTT bool) ([]Cue, []string) {
	var cues []Cue
	var warns []string
	i := 0
	sawBadTiming := false

	for i < len(lines) {
		// Skip blank lines between blocks.
		for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
			i++
		}
		if i >= len(lines) {
			break
		}

		first := strings.TrimSpace(lines[i])

		// VTT comment/config blocks: consume until blank line.
		if isVTT && (strings.HasPrefix(first, "NOTE") || first == "STYLE" || strings.HasPrefix(first, "REGION")) {
			for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
				i++
			}
			continue
		}

		// Optional counter (SRT) or cue identifier (VTT) line before the timing line.
		timingIdx := i
		if !arrowRe.MatchString(lines[i]) {
			if i+1 < len(lines) && arrowRe.MatchString(lines[i+1]) {
				timingIdx = i + 1
			} else {
				// Not a cue block; skip one line and warn once.
				if !sawBadTiming && first != "" {
					warns = append(warns, fmt.Sprintf("skipped unrecognized content near %q", clip(first, 40)))
					sawBadTiming = true
				}
				i++
				continue
			}
		}

		m := arrowRe.FindStringSubmatch(lines[timingIdx])
		startSec, ok1 := parseTime(m[1])
		endSec, ok2 := parseTime(m[2])
		i = timingIdx + 1

		// Collect text lines until blank line.
		var textLines []string
		for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
			ln := lines[i]
			if !srtPosRe.MatchString(ln) { // drop SRT X1/Y1 positioning extension lines
				textLines = append(textLines, ln)
			}
			i++
		}

		if !ok1 || !ok2 {
			warns = append(warns, fmt.Sprintf("cue with unparseable timestamp %q skipped", clip(m[0], 40)))
			continue
		}
		if endSec <= startSec {
			warns = append(warns, fmt.Sprintf("cue at %s has end <= start; skipped", fmtTime(startSec)))
			continue
		}
		if endSec-startSec > MaxCueDuration {
			warns = append(warns, fmt.Sprintf("cue at %s is unusually long (%.0fs)", fmtTime(startSec), endSec-startSec))
		}

		txt := cleanText(strings.Join(textLines, "\n"))
		if txt == "" {
			continue // empty cue: nothing to render
		}
		if utf8.RuneCountInString(txt) > MaxCueChars {
			r := []rune(txt)
			txt = string(r[:MaxCueChars])
			warns = append(warns, fmt.Sprintf("cue at %s truncated to %d characters", fmtTime(startSec), MaxCueChars))
		}

		cues = append(cues, Cue{Start: startSec, End: endSec, Text: txt})
	}
	return cues, warns
}

// parseTime converts "HH:MM:SS,mmm", "HH:MM:SS.mmm" or "MM:SS.mmm" to seconds.
func parseTime(s string) (float64, bool) {
	m := timeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	h := 0
	if m[1] != "" {
		h, _ = strconv.Atoi(m[1])
	}
	mi, _ := strconv.Atoi(m[2])
	se, _ := strconv.Atoi(m[3])
	msStr := m[4]
	for len(msStr) < 3 {
		msStr += "0" // "5" means 500ms per common practice
	}
	ms, _ := strconv.Atoi(msStr)
	if mi > 59 || se > 60 { // se==60 tolerated (leap-ish sloppy files)
		return 0, false
	}
	return float64(h)*3600 + float64(mi)*60 + float64(se) + float64(ms)/1000, true
}

// cleanText strips markup tags, decodes the handful of HTML entities that
// appear in real subtitle files, and normalizes whitespace per line.
func cleanText(s string) string {
	s = tagRe.ReplaceAllString(s, "")
	// Entities seen in the wild (SRT inherits them from HTML-ish authoring tools).
	repl := strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&#39;", "'", "&apos;", "'", "&nbsp;", " ",
	)
	s = repl.Replace(s)
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.Join(strings.Fields(ln), " ")
		if ln != "" {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

// decodeToUTF8 handles the encoding roulette of real-world SRT files:
// UTF-8 (with/without BOM), UTF-16 LE/BE (with BOM), and falls back to
// Windows-1252 for byte sequences that are not valid UTF-8. Deterministic:
// the same bytes always decode the same way.
func decodeToUTF8(b []byte) (string, string) {
	// UTF-16 BOMs.
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		return utf16Decode(b[2:], true), "decoded as UTF-16LE"
	}
	if len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF {
		return utf16Decode(b[2:], false), "decoded as UTF-16BE"
	}
	if utf8.Valid(b) {
		return string(b), ""
	}
	// Windows-1252 fallback: map 0x80–0x9F via table, rest = Latin-1.
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		sb.WriteRune(cp1252[c])
	}
	return sb.String(), "file was not valid UTF-8; decoded as Windows-1252"
}

func utf16Decode(b []byte, littleEndian bool) string {
	var sb strings.Builder
	var units []uint16
	for i := 0; i+1 < len(b); i += 2 {
		var u uint16
		if littleEndian {
			u = uint16(b[i]) | uint16(b[i+1])<<8
		} else {
			u = uint16(b[i])<<8 | uint16(b[i+1])
		}
		units = append(units, u)
	}
	for i := 0; i < len(units); i++ {
		u := units[i]
		switch {
		case u >= 0xD800 && u <= 0xDBFF && i+1 < len(units) && units[i+1] >= 0xDC00 && units[i+1] <= 0xDFFF:
			r := 0x10000 + (rune(u)-0xD800)<<10 + (rune(units[i+1]) - 0xDC00)
			sb.WriteRune(r)
			i++
		case u >= 0xD800 && u <= 0xDFFF:
			sb.WriteRune(utf8.RuneError)
		default:
			sb.WriteRune(rune(u))
		}
	}
	return sb.String()
}

func fmtTime(sec float64) string {
	h := int(sec) / 3600
	m := (int(sec) % 3600) / 60
	s := sec - float64(h*3600+m*60)
	return fmt.Sprintf("%02d:%02d:%06.3f", h, m, s)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// cp1252 maps every byte to its Windows-1252 rune (0x80–0x9F differ from Latin-1).
var cp1252 = func() [256]rune {
	var t [256]rune
	for i := 0; i < 256; i++ {
		t[i] = rune(i)
	}
	for b, r := range map[byte]rune{
		0x80: '€', 0x82: '‚', 0x83: 'ƒ', 0x84: '„', 0x85: '…', 0x86: '†',
		0x87: '‡', 0x88: 'ˆ', 0x89: '‰', 0x8A: 'Š', 0x8B: '‹', 0x8C: 'Œ',
		0x8E: 'Ž', 0x91: '‘', 0x92: '’', 0x93: '“', 0x94: '”', 0x95: '•',
		0x96: '–', 0x97: '—', 0x98: '˜', 0x99: '™', 0x9A: 'š', 0x9B: '›',
		0x9C: 'œ', 0x9E: 'ž', 0x9F: 'Ÿ',
	} {
		t[b] = r
	}
	return t
}()
