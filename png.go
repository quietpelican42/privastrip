package privastrip

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

const pngSignature = "\x89PNG\r\n\x1a\n"

// maxPNGChunkLength is the largest chunk length PNG permits (2^31-1 bytes,
// since the length field, though 4 bytes, must not have its high bit set).
const maxPNGChunkLength = 0x7FFFFFFF

// stripPNG parses a PNG stream (positioned at the 8-byte signature) from r,
// copying kept chunks to w (or discarding, if w is nil, for Detect) and
// dropping metadata chunks. Kept chunks, including their CRC, are copied
// byte-for-byte: chunks are independent, so removing others never
// invalidates a kept chunk's CRC. Chunk payloads are streamed through with a
// bounded internal buffer regardless of the declared chunk length, so a
// crafted huge length cannot force a large allocation.
func stripPNG(r *bufio.Reader, w io.Writer) (Report, error) {
	rep := Report{Format: FormatPNG}

	sig := make([]byte, 8)
	if _, err := io.ReadFull(r, sig); err != nil {
		return rep, fmt.Errorf("%w: reading PNG signature: %v", ErrCorrupt, err)
	}
	if string(sig) != pngSignature {
		return rep, fmt.Errorf("%w: bad PNG signature", ErrCorrupt)
	}
	if err := writeAll(w, sig); err != nil {
		return rep, err
	}

	for {
		var lenBuf [4]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			return rep, fmt.Errorf("%w: reading chunk length (missing IEND?): %v", ErrCorrupt, err)
		}
		length := binary.BigEndian.Uint32(lenBuf[:])
		if length > maxPNGChunkLength {
			return rep, fmt.Errorf("%w: chunk length %d exceeds PNG limit", ErrCorrupt, length)
		}

		typeBuf := make([]byte, 4)
		if _, err := io.ReadFull(r, typeBuf); err != nil {
			return rep, fmt.Errorf("%w: reading chunk type: %v", ErrCorrupt, err)
		}
		typ := string(typeBuf)

		drop, kind := classifyPNGChunk(typ)
		if drop {
			if _, err := io.CopyN(io.Discard, r, int64(length)+4); err != nil { // data + CRC
				return rep, fmt.Errorf("%w: reading %s chunk body: %v", ErrCorrupt, typ, err)
			}
			rep.Removed = append(rep.Removed, Item{Kind: kind, Marker: typ, Bytes: 8 + int(length) + 4})
		} else {
			if err := writeAll(w, lenBuf[:]); err != nil {
				return rep, err
			}
			if err := writeAll(w, typeBuf); err != nil {
				return rep, err
			}
			if _, err := discardOrCopy(w, r, int64(length)+4); err != nil {
				return rep, fmt.Errorf("%w: copying %s chunk body: %v", ErrCorrupt, typ, err)
			}
		}

		if typ == "IEND" {
			return rep, nil
		}
	}
}

// classifyPNGChunk reports whether a chunk type carries privacy-sensitive
// metadata and should be dropped, and if so, what kind.
//
// Dropped: eXIf (Exif), tEXt/zTXt/iTXt (text, may carry author, GPS via
// "XML:com.adobe.xmp", software, timestamps in free text), tIME (last
// modification time). Kept: everything else, including iCCP/sRGB/gAMA/cHRM
// (color rendering) and animation chunks (acTL/fcTL/fdAT), untouched.
func classifyPNGChunk(typ string) (drop bool, kind Kind) {
	switch typ {
	case "eXIf":
		return true, KindEXIF
	case "tEXt", "zTXt", "iTXt":
		return true, KindText
	case "tIME":
		return true, KindTimestamp
	default:
		return false, ""
	}
}
