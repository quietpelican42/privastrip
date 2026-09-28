package privastrip

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// webpChunk builds a raw WebP RIFF chunk: FourCC + little-endian size +
// payload + pad byte if the payload length is odd.
func webpChunk(fourcc string, payload []byte) []byte {
	var out bytes.Buffer
	out.WriteString(fourcc)
	var sizeBuf [4]byte
	binary.LittleEndian.PutUint32(sizeBuf[:], uint32(len(payload)))
	out.Write(sizeBuf[:])
	out.Write(payload)
	if len(payload)%2 == 1 {
		out.WriteByte(0)
	}
	return out.Bytes()
}

// buildTestWebP hand-assembles a minimal extended-format WebP: RIFF/WEBP
// header, a VP8X chunk with the given flags, and the given chunks after it
// (typically a VP8L "image data" chunk plus EXIF/XMP). privastrip never
// decodes WebP pixel data (the standard library has no WebP decoder), so
// the VP8L payload here does not need to be a real, decodable bitstream —
// only its bytes need to survive Strip unchanged, which is what the test
// checks.
func buildTestWebP(t *testing.T, vp8xFlags byte, chunks ...[]byte) []byte {
	t.Helper()
	vp8xPayload := make([]byte, 10)
	vp8xPayload[0] = vp8xFlags
	// bytes 1-3 reserved, 4-6 canvas width-1 (LE 24-bit) = 3 (width 4),
	// 7-9 canvas height-1 = 3 (height 4).
	vp8xPayload[4], vp8xPayload[5], vp8xPayload[6] = 3, 0, 0
	vp8xPayload[7], vp8xPayload[8], vp8xPayload[9] = 3, 0, 0

	var body bytes.Buffer
	body.WriteString("WEBP")
	body.Write(webpChunk("VP8X", vp8xPayload))
	for _, c := range chunks {
		body.Write(c)
	}

	var out bytes.Buffer
	out.WriteString("RIFF")
	var sizeBuf [4]byte
	binary.LittleEndian.PutUint32(sizeBuf[:], uint32(body.Len()))
	out.Write(sizeBuf[:])
	out.Write(body.Bytes())
	return out.Bytes()
}

func TestStripWebP_RemovesExifXmpAndClearsFlags(t *testing.T) {
	vp8lPayload := []byte("dummy-vp8l-bitstream-bytes-not-decoded")
	exifPayload := []byte("fake TIFF exif bytes for webp")
	xmpPayload := []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/"></x:xmpmeta>`)

	flags := byte(vp8xFlagEXIF | vp8xFlagXMP | 0x20) // + ICC bit, which we must NOT touch
	input := buildTestWebP(t, flags,
		webpChunk("VP8L", vp8lPayload),
		webpChunk("EXIF", exifPayload),
		webpChunk("XMP ", xmpPayload),
	)

	var out bytes.Buffer
	rep, err := Strip(bytes.NewReader(input), &out)
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	if len(rep.Removed) != 2 {
		t.Fatalf("removed %d items, want 2 (EXIF, XMP): %+v", len(rep.Removed), rep.Removed)
	}

	stripped := out.Bytes()

	// RIFF container must still be well-formed and its size field accurate.
	if string(stripped[0:4]) != "RIFF" || string(stripped[8:12]) != "WEBP" {
		t.Fatalf("output is not a valid RIFF/WEBP container")
	}
	declared := binary.LittleEndian.Uint32(stripped[4:8])
	if int(declared) != len(stripped)-8 {
		t.Fatalf("RIFF size field = %d, want %d", declared, len(stripped)-8)
	}

	// EXIF/XMP payloads must be gone.
	if bytes.Contains(stripped, exifPayload) {
		t.Errorf("EXIF payload still present")
	}
	if bytes.Contains(stripped, xmpPayload) {
		t.Errorf("XMP payload still present")
	}

	// VP8L "image data" must survive byte-for-byte.
	if !bytes.Contains(stripped, vp8lPayload) {
		t.Errorf("VP8L chunk payload was altered or removed")
	}

	// VP8X flags: EXIF and XMP bits cleared, ICC bit untouched.
	vp8xOff := bytes.Index(stripped, []byte("VP8X"))
	if vp8xOff < 0 {
		t.Fatalf("VP8X chunk missing from output")
	}
	gotFlags := stripped[vp8xOff+8]
	if gotFlags&vp8xFlagEXIF != 0 {
		t.Errorf("EXIF flag bit still set: 0x%02X", gotFlags)
	}
	if gotFlags&vp8xFlagXMP != 0 {
		t.Errorf("XMP flag bit still set: 0x%02X", gotFlags)
	}
	if gotFlags&0x20 == 0 {
		t.Errorf("ICC flag bit was cleared but should not have been: 0x%02X", gotFlags)
	}
}

func TestStripWebP_CleanFilePassesThroughFlagsUnset(t *testing.T) {
	vp8lPayload := []byte("some-vp8l-bytes")
	input := buildTestWebP(t, 0x00, webpChunk("VP8L", vp8lPayload))

	var out bytes.Buffer
	rep, err := Strip(bytes.NewReader(input), &out)
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	if rep.HasMetadata() {
		t.Fatalf("expected no metadata, got %+v", rep.Removed)
	}
	if !bytes.Equal(input, out.Bytes()) {
		t.Fatalf("clean WebP with no metadata chunks should pass through byte-for-byte")
	}
}

func TestDetectWebP_DoesNotModify(t *testing.T) {
	input := buildTestWebP(t, vp8xFlagEXIF,
		webpChunk("VP8L", []byte("x")),
		webpChunk("EXIF", []byte("exifdata")),
	)
	rep, err := Detect(bytes.NewReader(input))
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(rep.Removed) != 1 || rep.Removed[0].Kind != KindEXIF {
		t.Fatalf("unexpected Detect result: %+v", rep.Removed)
	}
}

func TestStripWebP_CorruptInput(t *testing.T) {
	cases := map[string][]byte{
		"not RIFF": []byte("not a webp file at all....."),
		"RIFF but not WEBP": func() []byte {
			var b bytes.Buffer
			b.WriteString("RIFF")
			b.Write([]byte{0, 0, 0, 4})
			b.WriteString("AVI ")
			return b.Bytes()
		}(),
		"truncated chunk header": func() []byte {
			var b bytes.Buffer
			b.WriteString("RIFF")
			b.Write([]byte{7, 0, 0, 0})
			b.WriteString("WEBP")
			b.WriteString("EXI") // partial chunk header: 3 of 8 bytes
			return b.Bytes()
		}(),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			_, err := Strip(bytes.NewReader(data), &out)
			if err == nil {
				t.Fatalf("expected error for %q", name)
			}
		})
	}
}
