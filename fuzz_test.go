package privastrip

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// FuzzStripJPEG feeds arbitrary bytes, seeded with a valid JPEG carrying
// EXIF/XMP/IPTC/ICC/comment segments, through Strip. It never checks output
// correctness (invalid input has none to check) — only that Strip does not
// panic and returns, i.e. that segment-length-driven reads stay bounded
// even against a corrupted length field.
func FuzzStripJPEG(f *testing.F) {
	seed, err := seedJPEG()
	if err != nil {
		f.Fatalf("building JPEG seed: %v", err)
	}
	f.Add(seed)
	f.Add([]byte{0xFF, 0xD8})
	f.Add([]byte{0xFF, 0xD8, 0xFF, 0xD9})
	f.Add([]byte("not a jpeg"))
	f.Fuzz(func(t *testing.T, data []byte) {
		var out bytes.Buffer
		_, _ = Strip(bytes.NewReader(data), &out)
	})
}

// FuzzStripPNG is FuzzStripJPEG's PNG counterpart.
func FuzzStripPNG(f *testing.F) {
	seed, err := seedPNG()
	if err != nil {
		f.Fatalf("building PNG seed: %v", err)
	}
	f.Add(seed)
	f.Add([]byte(pngSignature))
	f.Add([]byte("not a png"))
	f.Fuzz(func(t *testing.T, data []byte) {
		var out bytes.Buffer
		_, _ = Strip(bytes.NewReader(data), &out)
	})
}

// FuzzStripWebP is FuzzStripJPEG's WebP counterpart.
func FuzzStripWebP(f *testing.F) {
	f.Add(seedWebP())
	f.Add([]byte("RIFF\x00\x00\x00\x00WEBP"))
	f.Add([]byte("not a webp"))
	f.Fuzz(func(t *testing.T, data []byte) {
		var out bytes.Buffer
		_, _ = Strip(bytes.NewReader(data), &out)
	})
}

// seedJPEG builds a small valid JPEG with EXIF (including Orientation),
// XMP, IPTC, ICC, and a comment segment, without depending on *testing.T.
func seedJPEG() ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 60), uint8(y * 60), 100, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		return nil, err
	}
	base := buf.Bytes()

	var tiff bytes.Buffer
	tiff.WriteString("II")
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(42))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(8))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(1)) // 1 entry
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x0112))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(3))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(1))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(6))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(0))
	exif := segment(markerAPP1, append([]byte(exifSignature), tiff.Bytes()...))
	xmp := segment(markerAPP1, append([]byte(xmpSignature), []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/"></x:xmpmeta>`)...))
	iptc := segment(markerAPP13, append([]byte(photoshopIRBSignature), []byte("8BIMdata")...))
	icc := segment(markerAPP2, append([]byte(iccSignature), []byte{1, 2, 3, 4}...))
	com := segment(markerCOM, []byte("hello"))

	var out bytes.Buffer
	out.Write(base[:2]) // SOI
	out.Write(exif)
	out.Write(xmp)
	out.Write(iptc)
	out.Write(icc)
	out.Write(com)
	out.Write(base[2:])
	return out.Bytes(), nil
}

// seedPNG builds a small valid PNG with eXIf/tEXt/tIME chunks, without
// depending on *testing.T.
func seedPNG() ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	base := buf.Bytes()
	ihdrLen := int(binary.BigEndian.Uint32(base[8:12]))
	insertAt := 8 + 4 + 4 + ihdrLen + 4

	var out bytes.Buffer
	out.Write(base[:insertAt])
	out.Write(pngChunk("eXIf", []byte("exifbytes")))
	out.Write(pngChunk("tEXt", []byte("Author\x00Jane")))
	out.Write(pngChunk("tIME", []byte{0x07, 0xE8, 1, 1, 0, 0, 0}))
	out.Write(base[insertAt:])
	return out.Bytes(), nil
}

// seedWebP builds a small hand-assembled extended-format WebP with EXIF and
// XMP chunks, without depending on *testing.T.
func seedWebP() []byte {
	vp8x := make([]byte, 10)
	vp8x[0] = vp8xFlagEXIF | vp8xFlagXMP
	vp8x[4], vp8x[7] = 3, 3 // 4x4 canvas

	var body bytes.Buffer
	body.WriteString("WEBP")
	body.Write(webpChunk("VP8X", vp8x))
	body.Write(webpChunk("VP8L", []byte("dummy")))
	body.Write(webpChunk("EXIF", []byte("exifbytes")))
	body.Write(webpChunk("XMP ", []byte("<xmp/>")))

	var out bytes.Buffer
	out.WriteString("RIFF")
	var sizeBuf [4]byte
	binary.LittleEndian.PutUint32(sizeBuf[:], uint32(body.Len()))
	out.Write(sizeBuf[:])
	out.Write(body.Bytes())
	return out.Bytes()
}
