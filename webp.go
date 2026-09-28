package privastrip

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

// VP8X flag bits (RIFF container spec: Rsv Rsv ICC Alpha Exif XMP Anim Rsv,
// MSB to LSB).
const (
	vp8xFlagEXIF = 0x08
	vp8xFlagXMP  = 0x04
)

// maxVP8XChunkSize bounds the VP8X chunk payload read. The format defines it
// as exactly 10 bytes; this only guards against a crafted chunk size field
// forcing a large allocation.
const maxVP8XChunkSize = 1024

// stripWebP parses a WebP (RIFF) stream (positioned at "RIFF") from r,
// dropping EXIF and XMP chunks and clearing their VP8X flag bits, and
// recomputing the outer RIFF size. Every other chunk (VP8, VP8L, ICCP, ANIM,
// ANMF, ALPH, ...) is copied through byte-for-byte.
//
// Because the RIFF size field precedes the chunks it describes and w is a
// forward-only io.Writer, the whole re-serialized body is assembled in
// memory before anything is written. Output size never exceeds input size,
// so this is bounded by the input file's own size — not by anything an
// attacker controls independently of it — but it does mean a very large
// WebP (e.g. a long animation) is buffered whole; see the README.
func stripWebP(r *bufio.Reader, w io.Writer) (Report, error) {
	rep := Report{Format: FormatWebP}

	var header [12]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return rep, fmt.Errorf("%w: reading RIFF header: %v", ErrCorrupt, err)
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WEBP" {
		return rep, fmt.Errorf("%w: not a WebP file", ErrCorrupt)
	}

	var body bytes.Buffer
	body.WriteString("WEBP")

	for {
		var chdr [8]byte
		n, err := io.ReadFull(r, chdr[:])
		if err != nil {
			if (err == io.EOF || err == io.ErrUnexpectedEOF) && n == 0 {
				break
			}
			return rep, fmt.Errorf("%w: reading chunk header: %v", ErrCorrupt, err)
		}
		fourcc := string(chdr[0:4])
		size := binary.LittleEndian.Uint32(chdr[4:8])
		padded := int64(size)
		if size%2 == 1 {
			padded++
		}

		switch fourcc {
		case "EXIF", "XMP ":
			if _, err := io.CopyN(io.Discard, r, padded); err != nil {
				return rep, fmt.Errorf("%w: reading %s chunk: %v", ErrCorrupt, fourcc, err)
			}
			kind := KindEXIF
			if fourcc == "XMP " {
				kind = KindXMP
			}
			rep.Removed = append(rep.Removed, Item{
				Kind:   kind,
				Marker: strings.TrimSpace(fourcc),
				Bytes:  8 + int(padded),
			})

		case "VP8X":
			if size < 10 || size > maxVP8XChunkSize {
				return rep, fmt.Errorf("%w: implausible VP8X chunk size %d", ErrCorrupt, size)
			}
			payload := make([]byte, size)
			if _, err := io.ReadFull(r, payload); err != nil {
				return rep, fmt.Errorf("%w: reading VP8X chunk: %v", ErrCorrupt, err)
			}
			if size%2 == 1 {
				if _, err := io.CopyN(io.Discard, r, 1); err != nil {
					return rep, fmt.Errorf("%w: reading VP8X pad byte: %v", ErrCorrupt, err)
				}
			}
			payload[0] &^= vp8xFlagEXIF
			payload[0] &^= vp8xFlagXMP
			writeWebPChunk(&body, fourcc, payload)

		default:
			writeWebPChunkHeader(&body, fourcc, size)
			if _, err := io.CopyN(&body, r, padded); err != nil {
				return rep, fmt.Errorf("%w: reading %s chunk body: %v", ErrCorrupt, fourcc, err)
			}
		}
	}

	if body.Len() < 4 {
		return rep, fmt.Errorf("%w: no chunks found", ErrCorrupt)
	}

	if w != nil {
		if err := writeAll(w, []byte("RIFF")); err != nil {
			return rep, err
		}
		var sizeBuf [4]byte
		binary.LittleEndian.PutUint32(sizeBuf[:], uint32(body.Len()))
		if err := writeAll(w, sizeBuf[:]); err != nil {
			return rep, err
		}
		if err := writeAll(w, body.Bytes()); err != nil {
			return rep, err
		}
	}

	return rep, nil
}

// writeWebPChunkHeader writes a chunk's FourCC and little-endian size.
func writeWebPChunkHeader(buf *bytes.Buffer, fourcc string, size uint32) {
	buf.WriteString(fourcc)
	var sizeBuf [4]byte
	binary.LittleEndian.PutUint32(sizeBuf[:], size)
	buf.Write(sizeBuf[:])
}

// writeWebPChunk writes a complete chunk (header, payload, and pad byte if
// the payload length is odd).
func writeWebPChunk(buf *bytes.Buffer, fourcc string, payload []byte) {
	writeWebPChunkHeader(buf, fourcc, uint32(len(payload)))
	buf.Write(payload)
	if len(payload)%2 == 1 {
		buf.WriteByte(0)
	}
}
