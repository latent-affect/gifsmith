package main

// gifmeta.go — strip GIF Comment Extension blocks from encoder output.
//
// gifski (this project's default encoder) hardcodes a "gif.ski" Comment
// Extension into every file it writes, with no CLI flag to suppress it
// (verified: `gifski --help` against the pinned 1.34.0 binary has no such
// option). That is a software fingerprint riding along in the output — the
// exact opposite of the "nothing traces back to how or where this was
// made" claim this project wants to be able to make. The FFmpeg palette
// path does not currently add one, but nothing guarantees that stays true
// across FFmpeg versions, so both paths go through the same strip.
//
// stripGIFComments does a real structural walk of the GIF89a/87a block
// stream rather than a blind byte-pattern removal: GIF's LZW-compressed
// image data is high-entropy and will contain the two-byte sequence 0x21
// 0xFE (the Comment Extension introducer) by pure chance in a file of any
// real size — confirmed empirically against this project's own test
// output, where that pattern turned up 4-5 times per file and only one was
// a real comment block. A blind strip would corrupt pixel data; parsing
// every block and only dropping genuine top-level Comment Extensions is
// the only safe way to do this.

import "fmt"

// stripGIFComments returns data with every Comment Extension block (0x21
// 0xFE ...) removed, and whether anything was actually removed. On any
// parse failure it returns the original data unchanged with changed=false
// and a non-nil error — callers should treat that as "leave the GIF alone"
// rather than fail the encode over a cosmetic cleanup step.
func stripGIFComments(data []byte) (out []byte, changed bool, err error) {
	if len(data) < 13 || (string(data[0:6]) != "GIF87a" && string(data[0:6]) != "GIF89a") {
		return data, false, fmt.Errorf("not a GIF file (bad header)")
	}

	out = make([]byte, 0, len(data))
	out = append(out, data[0:6]...)
	pos := 6

	if pos+7 > len(data) {
		return data, false, fmt.Errorf("truncated logical screen descriptor")
	}
	packed := data[pos+4]
	out = append(out, data[pos:pos+7]...)
	pos += 7

	if packed&0x80 != 0 { // global color table present
		size := 3 * (1 << ((packed & 0x07) + 1))
		if pos+size > len(data) {
			return data, false, fmt.Errorf("truncated global color table")
		}
		out = append(out, data[pos:pos+size]...)
		pos += size
	}

	// readSubBlocks scans a length-prefixed sub-block series starting at n
	// (each: 1 length byte, that many data bytes) up to and including its
	// zero-length terminator, returning the position just past it.
	readSubBlocks := func(n int) (int, error) {
		for {
			if n >= len(data) {
				return 0, fmt.Errorf("truncated sub-blocks at %d", n)
			}
			l := int(data[n])
			n++
			if l == 0 {
				return n, nil
			}
			n += l
			if n > len(data) {
				return 0, fmt.Errorf("truncated sub-block data at %d", n)
			}
		}
	}

	for pos < len(data) {
		switch b := data[pos]; b {
		case 0x21: // extension introducer
			if pos+2 >= len(data) {
				return data, false, fmt.Errorf("truncated extension at %d", pos)
			}
			label := data[pos+1]
			switch label {
			case 0xF9: // Graphic Control Extension: fixed 4-byte block + terminator
				blockSize := int(data[pos+2])
				end := pos + 3 + blockSize + 1
				if end > len(data) {
					return data, false, fmt.Errorf("truncated graphic control extension at %d", pos)
				}
				out = append(out, data[pos:end]...)
				pos = end
			case 0xFE: // Comment Extension: DROP, keep nothing
				n, err := readSubBlocks(pos + 2)
				if err != nil {
					return data, false, err
				}
				pos = n
				changed = true
			case 0x01, 0xFF: // Plain Text / Application Extension: fixed header + sub-blocks
				blockSize := int(data[pos+2])
				n := pos + 3 + blockSize
				if n > len(data) {
					return data, false, fmt.Errorf("truncated extension 0x%02x at %d", label, pos)
				}
				n, err := readSubBlocks(n)
				if err != nil {
					return data, false, err
				}
				out = append(out, data[pos:n]...)
				pos = n
			default:
				return data, false, fmt.Errorf("unknown extension label 0x%02x at %d", label, pos)
			}
		case 0x2C: // Image Descriptor
			if pos+10 > len(data) {
				return data, false, fmt.Errorf("truncated image descriptor at %d", pos)
			}
			idPacked := data[pos+9]
			n := pos + 10
			if idPacked&0x80 != 0 { // local color table present
				n += 3 * (1 << ((idPacked & 0x07) + 1))
			}
			if n >= len(data) {
				return data, false, fmt.Errorf("truncated before LZW min code size at %d", n)
			}
			n++ // LZW minimum code size byte
			n, err := readSubBlocks(n)
			if err != nil {
				return data, false, err
			}
			out = append(out, data[pos:n]...)
			pos = n
		case 0x3B: // Trailer
			out = append(out, b)
			pos++
		default:
			return data, false, fmt.Errorf("unexpected block introducer 0x%02x at %d", b, pos)
		}
	}
	return out, changed, nil
}
