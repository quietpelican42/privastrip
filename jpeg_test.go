package privastrip

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// buildTestJPEG returns a small valid JPEG (SOI, APP0/JFIF, SOF/DHT/DQT/SOS
// from the real encoder, EOI) plus, spliced in right after APP0, the given
// extra segments (each already a complete marker+length+payload byte slice).
func buildTestJPEG(t *testing.T, extraSegments ...[]byte) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 30), uint8(y * 30), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encoding base JPEG: %v", err)
	}
	base := buf.Bytes()

	// Insert right after SOI (2 bytes); everything else the encoder wrote
	// (DQT, SOF, DHT, SOS, scan data, EOI) follows, untouched.
	if base[0] != 0xFF || base[1] != 0xD8 {
		t.Fatalf("encoder did not produce SOI")
	}
	insertAt := 2

	var out bytes.Buffer
	out.Write(base[:insertAt])
	for _, seg := range extraSegments {
		out.Write(seg)
	}
	out.Write(base[insertAt:])
	return out.Bytes()
}

// segment builds a raw marker+length+payload segment.
func segment(code byte, payload []byte) []byte {
	length := len(payload) + 2
	out := make([]byte, 0, 2+2+len(payload))
	out = append(out, 0xFF, code)
	out = append(out, byte(length>>8), byte(length))
	out = append(out, payload...)
	return out
}

func exifSegment(t *testing.T, orientation uint16, hasOrientation bool) []byte {
	t.Helper()
	var tiff bytes.Buffer
	tiff.WriteString("II")
	binary.Write(&tiff, binary.LittleEndian, uint16(42))
	binary.Write(&tiff, binary.LittleEndian, uint32(8))
	if hasOrientation {
		binary.Write(&tiff, binary.LittleEndian, uint16(1)) // 1 entry
		binary.Write(&tiff, binary.LittleEndian, uint16(0x0112))
		binary.Write(&tiff, binary.LittleEndian, uint16(3)) // SHORT
		binary.Write(&tiff, binary.LittleEndian, uint32(1)) // count
		binary.Write(&tiff, binary.LittleEndian, orientation)
		binary.Write(&tiff, binary.LittleEndian, uint16(0)) // pad
	} else {
		binary.Write(&tiff, binary.LittleEndian, uint16(0)) // 0 entries
	}
	binary.Write(&tiff, binary.LittleEndian, uint32(0)) // next IFD
	payload := append([]byte(exifSignature), tiff.Bytes()...)
	return segment(markerAPP1, payload)
}

func xmpSegment(xml string) []byte {
	payload := append([]byte(xmpSignature), []byte(xml)...)
	return segment(markerAPP1, payload)
}

func iptcSegment(data []byte) []byte {
	payload := append([]byte(photoshopIRBSignature), data...)
	return segment(markerAPP13, payload)
}

func iccSegment(data []byte) []byte {
	payload := append([]byte(iccSignature), data...)
	return segment(markerAPP2, payload)
}

func comSegment(text string) []byte {
	return segment(markerCOM, []byte(text))
}

func decodePixels(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return img
}

func requirePixelsEqual(t *testing.T, a, b image.Image) {
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
				t.Fatalf("pixel (%d,%d) differs: %v vs %v", x, y, a.At(x, y), b.At(x, y))
			}
		}
	}
}

func TestStripJPEG_RemovesExifXmpIptcComment(t *testing.T) {
	input := buildTestJPEG(t,
		exifSegment(t, 1, true),
		xmpSegment(`<x:xmpmeta xmlns:x="adobe:ns:meta/"></x:xmpmeta>`),
		iptcSegment([]byte("8BIMfakeIPTCdata")),
		comSegment("this is a comment"),
	)

	var out bytes.Buffer
	rep, err := Strip(bytes.NewReader(input), &out)
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	if rep.Format != FormatJPEG {
		t.Fatalf("Format = %v, want JPEG", rep.Format)
	}

	kinds := map[Kind]bool{}
	for _, item := range rep.Removed {
		kinds[item.Kind] = true
	}
	for _, want := range []Kind{KindEXIF, KindXMP, KindIPTC, KindComment} {
		if !kinds[want] {
			t.Errorf("expected %s to be reported removed, got %+v", want, rep.Removed)
		}
	}

	stripped := out.Bytes()
	for _, sig := range []string{exifSignature, "http://ns.adobe.com/xap/1.0/", "8BIM", "this is a comment"} {
		if bytes.Contains(stripped, []byte(sig)) {
			// Orientation preservation writes a tiny synthetic EXIF block
			// containing "Exif\0\0" itself, so allow that one exception.
			if sig == exifSignature && rep.OrientationPreserved {
				continue
			}
			t.Errorf("stripped output still contains %q", sig)
		}
	}

	// Pixel data must decode identically.
	requirePixelsEqual(t, decodePixels(t, input), decodePixels(t, stripped))
}

func TestStripJPEG_KeepsICCByDefault(t *testing.T) {
	iccData := bytes.Repeat([]byte{0x01, 0x02, 0x03, 0x04}, 20)
	input := buildTestJPEG(t, iccSegment(iccData))

	var out bytes.Buffer
	rep, err := Strip(bytes.NewReader(input), &out)
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	if rep.HasMetadata() {
		t.Fatalf("expected no metadata removed by default, got %+v", rep.Removed)
	}
	if !bytes.Contains(out.Bytes(), []byte(iccSignature)) {
		t.Fatalf("ICC profile was removed but should be kept by default")
	}
}

func TestStripJPEG_StripICCOption(t *testing.T) {
	iccData := bytes.Repeat([]byte{0x01, 0x02, 0x03, 0x04}, 20)
	input := buildTestJPEG(t, iccSegment(iccData))

	var out bytes.Buffer
	rep, err := Strip(bytes.NewReader(input), &out, WithStripICC(true))
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	found := false
	for _, item := range rep.Removed {
		if item.Kind == KindICC {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected ICC to be removed with WithStripICC(true), got %+v", rep.Removed)
	}
	if bytes.Contains(out.Bytes(), []byte(iccSignature)) {
		t.Fatalf("ICC profile still present after WithStripICC(true)")
	}
}

func TestStripJPEG_OrientationPreservedByDefault(t *testing.T) {
	input := buildTestJPEG(t, exifSegment(t, 6, true))

	var out bytes.Buffer
	rep, err := Strip(bytes.NewReader(input), &out)
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	if !rep.OrientationPreserved {
		t.Fatalf("expected OrientationPreserved = true")
	}

	// Decode the synthetic EXIF block back out and confirm orientation is 6.
	got, err := Detect(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatalf("Detect on stripped output: %v", err)
	}
	if got.HasMetadata() {
		// The synthetic block should be detected as EXIF metadata again,
		// which is expected (it IS an EXIF segment) - re-stripping would
		// remove it (and its Orientation) too, which is fine: it's an
		// explicit, minimal, single-tag block, not the original data.
	}

	found := false
	for i := 0; i+3 < len(out.Bytes()); i++ {
		if out.Bytes()[i] == 0xFF && out.Bytes()[i+1] == markerAPP1 {
			payload := out.Bytes()[i+4:]
			if hasPrefix(payload, exifSignature) {
				tiff := payload[len(exifSignature):]
				val := orientationValue(tiff)
				if val != 6 {
					t.Fatalf("synthetic EXIF orientation = %d, want 6", val)
				}
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("no synthetic EXIF APP1 segment found in output")
	}
}

func TestStripJPEG_KeepOrientationFalseDropsEverything(t *testing.T) {
	input := buildTestJPEG(t, exifSegment(t, 6, true))

	var out bytes.Buffer
	rep, err := Strip(bytes.NewReader(input), &out, WithKeepOrientation(false))
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	if rep.OrientationPreserved {
		t.Fatalf("expected OrientationPreserved = false")
	}
	if bytes.Contains(out.Bytes(), []byte(exifSignature)) {
		t.Fatalf("EXIF signature still present after WithKeepOrientation(false)")
	}
}

func TestStripJPEG_CleanFileUnchanged(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode: %v", err)
	}
	input := buf.Bytes()

	var out bytes.Buffer
	rep, err := Strip(bytes.NewReader(input), &out)
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	if rep.HasMetadata() {
		t.Fatalf("expected clean encoder output to have no metadata, got %+v", rep.Removed)
	}
	requirePixelsEqual(t, decodePixels(t, input), decodePixels(t, out.Bytes()))
}

func TestDetectJPEG_DoesNotModify(t *testing.T) {
	input := buildTestJPEG(t, exifSegment(t, 1, true), comSegment("hello"))
	rep, err := Detect(bytes.NewReader(input))
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(rep.Removed) != 2 {
		t.Fatalf("Detect found %d items, want 2: %+v", len(rep.Removed), rep.Removed)
	}
	if rep.BytesOut != rep.BytesIn {
		t.Fatalf("Detect should not report any output bytes distinct from input: in=%d out=%d", rep.BytesIn, rep.BytesOut)
	}
}

func TestStripJPEG_UnrecognizedAPP1Kept(t *testing.T) {
	// An APP1 payload that isn't Exif or XMP should be left alone.
	input := buildTestJPEG(t, segment(markerAPP1, []byte("not a known signature at all")))
	var out bytes.Buffer
	rep, err := Strip(bytes.NewReader(input), &out)
	if err != nil {
		t.Fatalf("Strip: %v", err)
	}
	if rep.HasMetadata() {
		t.Fatalf("expected unrecognized APP1 to be kept, got removed: %+v", rep.Removed)
	}
	if !bytes.Contains(out.Bytes(), []byte("not a known signature at all")) {
		t.Fatalf("unrecognized APP1 payload was dropped")
	}
}

func TestStripJPEG_CorruptInput(t *testing.T) {
	cases := map[string][]byte{
		"truncated after SOI":       {0xFF, 0xD8},
		"bad SOI":                   {0xFF, 0xD9, 0x00, 0x00},
		"segment length too small":  {0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x01},
		"truncated segment payload": {0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x10, 0x01, 0x02},
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
