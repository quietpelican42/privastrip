package privastrip

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// pngChunk builds a raw chunk: length + type + data + crc.
func pngChunk(typ string, data []byte) []byte {
	var out bytes.Buffer
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	out.Write(lenBuf[:])
	out.WriteString(typ)
	out.Write(data)
	crc := crc32.NewIEEE()
	crc.Write([]byte(typ))
	crc.Write(data)
	var crcBuf [4]byte
	binary.BigEndian.PutUint32(crcBuf[:], crc.Sum32())
	out.Write(crcBuf[:])
	return out.Bytes()
}

// buildTestPNG encodes a small real PNG, then splices extraChunks in right
// after IHDR (before IDAT).
func buildTestPNG(t *testing.T, extraChunks ...[]byte) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 6, 6))
	for y := 0; y < 6; y++ {
		for x := 0; x < 6; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 40), uint8(y * 40), 200, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding base PNG: %v", err)
	}
	base := buf.Bytes()

	if string(base[:8]) != pngSignature {
		t.Fatalf("encoder did not produce PNG signature")
	}
	ihdrLen := int(binary.BigEndian.Uint32(base[8:12]))
	insertAt := 8 + 4 + 4 + ihdrLen + 4 // sig + len + type + data + crc

	var out bytes.Buffer
	out.Write(base[:insertAt])
	for _, c := range extraChunks {
		out.Write(c)
	}
	out.Write(base[insertAt:])
	return out.Bytes()
}

func decodePNG(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decoding PNG: %v", err)
	}
	return img
}

func TestStripPNG_RemovesMetadataChunks(t *testing.T) {
	input := buildTestPNG(t,
		pngChunk("eXIf", []byte("fake TIFF exif bytes")),
		pngChunk("tEXt", append([]byte("Author\x00"), []byte("Jane Doe")...)),
		pngChunk("zTXt", append([]byte("Comment\x00\x00"), []byte("compressed-ish")...)),
		pngChunk("iTXt", append([]byte("XML:com.adobe.xmp\x00\x00\x00\x00\x00"), []byte("<xmp/>")...)),
		pngChunk("tIME", []byte{0x07, 0xE8, 0x01, 0x01, 0x00, 0x00, 0x00}),
	)

	var out bytes.Buffer
	rep, err := Strip(bytes.NewReader(input), &out)
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	if len(rep.Removed) != 5 {
		t.Fatalf("removed %d chunks, want 5: %+v", len(rep.Removed), rep.Removed)
	}

	stripped := out.Bytes()
	for _, typ := range []string{"eXIf", "tEXt", "zTXt", "iTXt", "tIME"} {
		if bytes.Contains(stripped, []byte(typ)) {
			t.Errorf("stripped output still contains chunk type %q", typ)
		}
	}

	requirePixelsEqualGeneric(t, decodePNG(t, input), decodePNG(t, stripped))
}

func TestStripPNG_KeepsColorChunks(t *testing.T) {
	gammaData := make([]byte, 4)
	binary.BigEndian.PutUint32(gammaData, 45455)
	input := buildTestPNG(t, pngChunk("gAMA", gammaData))

	var out bytes.Buffer
	rep, err := Strip(bytes.NewReader(input), &out)
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	if rep.HasMetadata() {
		t.Fatalf("gAMA should not be reported as removed metadata: %+v", rep.Removed)
	}
	if !bytes.Contains(out.Bytes(), []byte("gAMA")) {
		t.Fatalf("gAMA chunk was dropped but should be kept")
	}
}

func TestStripPNG_CleanFileUnchanged(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 3, 3))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	input := buf.Bytes()

	var out bytes.Buffer
	rep, err := Strip(bytes.NewReader(input), &out)
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	if rep.HasMetadata() {
		t.Fatalf("expected no metadata in clean encoder output, got %+v", rep.Removed)
	}
	if !bytes.Equal(input, out.Bytes()) {
		t.Fatalf("clean PNG should pass through byte-for-byte")
	}
}

func TestStripPNG_CorruptInput(t *testing.T) {
	cases := map[string][]byte{
		"bad signature":       []byte("not a png at all............"),
		"truncated signature": []byte(pngSignature)[:4],
		"missing IEND":        append([]byte(pngSignature), pngChunk("IHDR", make([]byte, 13))...),
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

func TestDetectPNG_DoesNotModify(t *testing.T) {
	input := buildTestPNG(t, pngChunk("tEXt", append([]byte("Author\x00"), []byte("Jane")...)))
	rep, err := Detect(bytes.NewReader(input))
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(rep.Removed) != 1 || rep.Removed[0].Kind != KindText {
		t.Fatalf("unexpected Detect result: %+v", rep.Removed)
	}
}

func requirePixelsEqualGeneric(t *testing.T, a, b image.Image) {
	t.Helper()
	if a.Bounds() != b.Bounds() {
		t.Fatalf("bounds differ: %v vs %v", a.Bounds(), b.Bounds())
	}
	bounds := a.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			ar, ag, ab, aa := a.At(x, y).RGBA()
			br, bg, bb, ba := b.At(x, y).RGBA()
			if ar != br || ag != bg || ab != bb || aa != ba {
				t.Fatalf("pixel (%d,%d) differs", x, y)
			}
		}
	}
}
