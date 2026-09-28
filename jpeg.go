package privastrip

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// JPEG marker codes relevant to segment classification.
const (
	markerSOI   = 0xD8
	markerEOI   = 0xD9
	markerSOS   = 0xDA
	markerTEM   = 0x01
	markerRST0  = 0xD0
	markerRST7  = 0xD7
	markerAPP0  = 0xE0
	markerAPP1  = 0xE1
	markerAPP2  = 0xE2
	markerAPP13 = 0xED
	markerCOM   = 0xFE
)

const (
	exifSignature         = "Exif\x00\x00"
	xmpSignature          = "http://ns.adobe.com/xap/1.0/\x00"
	xmpExtendedSignature  = "http://ns.adobe.com/xmp/extension/\x00"
	iccSignature          = "ICC_PROFILE\x00"
	photoshopIRBSignature = "Photoshop 3.0\x00"
)

// maxJPEGSegment is the largest possible JPEG segment payload: a 2-byte
// length field that includes itself caps the whole segment at 65535 bytes.
const maxJPEGSegment = 65535

// stripJPEG parses a JPEG stream (positioned at the SOI marker) from r,
// copying kept segments to w (or discarding, if w is nil, for Detect) and
// dropping metadata segments. Once the first SOS (start-of-scan) marker is
// reached, the remainder of the stream is copied through byte-for-byte,
// since compressed scan data and any markers after it are never inspected.
func stripJPEG(r *bufio.Reader, w io.Writer, o Options) (Report, error) {
	rep := Report{Format: FormatJPEG}

	soi := make([]byte, 2)
	if _, err := io.ReadFull(r, soi); err != nil {
		return rep, fmt.Errorf("%w: reading SOI: %v", ErrCorrupt, err)
	}
	if soi[0] != 0xFF || soi[1] != markerSOI {
		return rep, fmt.Errorf("%w: missing SOI marker", ErrCorrupt)
	}
	if err := writeAll(w, soi); err != nil {
		return rep, err
	}

	var pendingOrientation []byte // set when an EXIF block with Orientation is dropped

	for {
		code, err := readMarkerCode(r)
		if err != nil {
			if err == io.EOF {
				// Truncated before SOS or EOI. Best-effort: not a valid JPEG.
				return rep, fmt.Errorf("%w: truncated before end of headers", ErrCorrupt)
			}
			return rep, err
		}

		switch {
		case code == markerEOI:
			if err := writeAll(w, []byte{0xFF, markerEOI}); err != nil {
				return rep, err
			}
			return rep, nil

		case code == markerTEM || (code >= markerRST0 && code <= markerRST7):
			// Standalone markers, no length/payload.
			if err := writeAll(w, []byte{0xFF, code}); err != nil {
				return rep, err
			}

		case code == markerSOS:
			header, err := readSegment(r, code)
			if err != nil {
				return rep, err
			}
			if o.keepOrientation && len(pendingOrientation) > 0 {
				block := buildMinimalEXIF(pendingOrientation)
				if err := writeAll(w, block); err != nil {
					return rep, err
				}
				rep.OrientationPreserved = true
				pendingOrientation = nil
			}
			if err := writeAll(w, header); err != nil {
				return rep, err
			}
			// From here on, copy the rest of the stream (entropy-coded scan
			// data, any further scans, and EOI) through unchanged.
			_, err = discardOrCopy(w, r, maxInt64)
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				return rep, fmt.Errorf("%w: copying scan data: %v", ErrCorrupt, err)
			}
			return rep, nil

		default:
			payload, raw, err := readSegmentPayload(r, code)
			if err != nil {
				return rep, err
			}
			keep, item, orientationPayload := classifyJPEGSegment(code, payload, o)
			if orientationPayload != nil {
				pendingOrientation = orientationPayload
			}
			if keep {
				if err := writeAll(w, raw); err != nil {
					return rep, err
				}
			} else {
				rep.Removed = append(rep.Removed, item)
			}
		}
	}
}

const maxInt64 = int64(^uint64(0) >> 1)

// readMarkerCode reads the next marker code, skipping any 0xFF fill bytes
// that may precede it per the JPEG spec.
func readMarkerCode(r *bufio.Reader) (byte, error) {
	b, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	if b != 0xFF {
		return 0, fmt.Errorf("%w: expected marker, got 0x%02X", ErrCorrupt, b)
	}
	for {
		b, err = r.ReadByte()
		if err != nil {
			return 0, err
		}
		if b != 0xFF {
			break
		}
	}
	if b == 0x00 {
		return 0, fmt.Errorf("%w: stray stuffed byte outside scan data", ErrCorrupt)
	}
	return b, nil
}

// readSegment reads a length-prefixed segment (2-byte big-endian length,
// including itself) and returns the raw bytes: marker + length + payload.
func readSegment(r *bufio.Reader, code byte) ([]byte, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("%w: reading segment length: %v", ErrCorrupt, err)
	}
	length := binary.BigEndian.Uint16(lenBuf[:])
	if length < 2 {
		return nil, fmt.Errorf("%w: invalid segment length %d", ErrCorrupt, length)
	}
	payloadLen := int(length) - 2
	buf := make([]byte, 2+2+payloadLen)
	buf[0], buf[1] = 0xFF, code
	buf[2], buf[3] = lenBuf[0], lenBuf[1]
	if payloadLen > 0 {
		if _, err := io.ReadFull(r, buf[4:]); err != nil {
			return nil, fmt.Errorf("%w: reading segment payload: %v", ErrCorrupt, err)
		}
	}
	return buf, nil
}

// readSegmentPayload reads a length-prefixed segment and returns both its
// payload alone and the full raw bytes (marker + length + payload). Segment
// length is capped at 65535 by the format's 16-bit length field, so this
// allocation is always bounded regardless of untrusted input.
func readSegmentPayload(r *bufio.Reader, code byte) (payload, raw []byte, err error) {
	raw, err = readSegment(r, code)
	if err != nil {
		return nil, nil, err
	}
	return raw[4:], raw, nil
}

// classifyJPEGSegment decides whether a non-SOS segment should be kept, and
// if dropped, what kind of metadata it was. When the segment is a full EXIF
// block containing an Orientation tag, orientationPayload returns that
// block's raw TIFF payload (the bytes after "Exif\0\0") for the caller to
// rebuild a minimal synthetic EXIF block from, if WithKeepOrientation is set.
func classifyJPEGSegment(code byte, payload []byte, o Options) (keep bool, item Item, orientationPayload []byte) {
	markerName := fmt.Sprintf("APP%d", code-0xE0)
	if code == markerCOM {
		markerName = "COM"
	}

	switch code {
	case markerAPP1:
		switch {
		case hasPrefix(payload, exifSignature):
			if ori, ok := extractOrientation(payload); ok {
				orientationPayload = ori
			}
			return false, Item{Kind: KindEXIF, Marker: markerName, Bytes: len(payload) + 4}, orientationPayload
		case hasPrefix(payload, xmpExtendedSignature):
			return false, Item{Kind: KindXMPExtended, Marker: markerName, Bytes: len(payload) + 4}, nil
		case hasPrefix(payload, xmpSignature):
			return false, Item{Kind: KindXMP, Marker: markerName, Bytes: len(payload) + 4}, nil
		default:
			// Unrecognized APP1 payload. Real-world APP1 is used almost
			// exclusively for Exif/XMP, but out of caution for lossless
			// decode we only remove what we can positively identify.
			return true, Item{}, nil
		}

	case markerAPP2:
		if hasPrefix(payload, iccSignature) && o.stripICC {
			return false, Item{Kind: KindICC, Marker: markerName, Bytes: len(payload) + 4}, nil
		}
		return true, Item{}, nil

	case markerAPP13:
		if hasPrefix(payload, photoshopIRBSignature) {
			return false, Item{Kind: KindIPTC, Marker: markerName, Bytes: len(payload) + 4}, nil
		}
		return true, Item{}, nil

	case markerCOM:
		return false, Item{Kind: KindComment, Marker: markerName, Bytes: len(payload) + 4}, nil

	default:
		// APP0 (JFIF), APP3-12/14/15, DQT, DHT, SOF*, DRI, etc: needed to
		// decode, or not identifiable as privacy metadata. Keep.
		return true, Item{}, nil
	}
}

func hasPrefix(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && string(b[:len(prefix)]) == prefix
}

// extractOrientation looks for tag 0x0112 (Orientation) in IFD0 of an EXIF
// TIFF payload (the bytes after "Exif\0\0"). It returns the raw TIFF payload
// unchanged if found (buildMinimalEXIF re-derives just the value), or ok=false.
func extractOrientation(exifPayload []byte) (tiff []byte, ok bool) {
	if !hasPrefix(exifPayload, exifSignature) {
		return nil, false
	}
	t := exifPayload[len(exifSignature):]
	if len(t) < 8 {
		return nil, false
	}
	var bo binary.ByteOrder
	switch string(t[0:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return nil, false
	}
	if bo.Uint16(t[2:4]) != 42 {
		return nil, false
	}
	ifdOffset := bo.Uint32(t[4:8])
	if int64(ifdOffset)+2 > int64(len(t)) {
		return nil, false
	}
	count := bo.Uint16(t[ifdOffset : ifdOffset+2])
	entriesStart := int64(ifdOffset) + 2
	for i := int64(0); i < int64(count); i++ {
		off := entriesStart + i*12
		if off+12 > int64(len(t)) {
			break
		}
		tag := bo.Uint16(t[off : off+2])
		if tag == 0x0112 {
			typ := bo.Uint16(t[off+2 : off+4])
			if typ != 3 { // SHORT
				return nil, false
			}
			return t, true
		}
	}
	return nil, false
}

// orientationValue reads the Orientation SHORT value out of a TIFF payload
// already confirmed (by extractOrientation) to contain tag 0x0112.
func orientationValue(t []byte) uint16 {
	var bo binary.ByteOrder
	if string(t[0:2]) == "II" {
		bo = binary.LittleEndian
	} else {
		bo = binary.BigEndian
	}
	ifdOffset := bo.Uint32(t[4:8])
	count := bo.Uint16(t[ifdOffset : ifdOffset+2])
	entriesStart := int64(ifdOffset) + 2
	for i := int64(0); i < int64(count); i++ {
		off := entriesStart + i*12
		tag := bo.Uint16(t[off : off+2])
		if tag == 0x0112 {
			return bo.Uint16(t[off+8 : off+10])
		}
	}
	return 1
}

// buildMinimalEXIF builds a complete APP1 EXIF segment (marker + length +
// payload) containing only IFD0's Orientation tag, little-endian, so a
// viewer that honors EXIF orientation still rotates the image correctly
// after the original (and possibly much larger) EXIF block has been
// removed. tiffPayload is the original TIFF payload extractOrientation
// confirmed contains a valid Orientation entry.
func buildMinimalEXIF(tiffPayload []byte) []byte {
	orientation := orientationValue(tiffPayload)

	tiff := make([]byte, 8+2+12+4)
	// TIFF header: little-endian, magic 42, IFD0 at offset 8.
	tiff[0], tiff[1] = 'I', 'I'
	binary.LittleEndian.PutUint16(tiff[2:4], 42)
	binary.LittleEndian.PutUint32(tiff[4:8], 8)
	// IFD0: 1 entry.
	binary.LittleEndian.PutUint16(tiff[8:10], 1)
	entry := tiff[10:22]
	binary.LittleEndian.PutUint16(entry[0:2], 0x0112) // Orientation
	binary.LittleEndian.PutUint16(entry[2:4], 3)      // type SHORT
	binary.LittleEndian.PutUint32(entry[4:8], 1)      // count 1
	binary.LittleEndian.PutUint16(entry[8:10], orientation)
	// entry[10:12] left zero (padding)
	// next IFD offset = 0
	binary.LittleEndian.PutUint32(tiff[22:26], 0)

	payload := append([]byte(exifSignature), tiff...)
	length := len(payload) + 2
	out := make([]byte, 0, 4+len(payload))
	out = append(out, 0xFF, markerAPP1)
	out = append(out, byte(length>>8), byte(length))
	out = append(out, payload...)
	return out
}
