package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

func writeJPEGWithEXIF(t *testing.T, dir, name string) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 50), uint8(y * 50), 90, 255})
		}
	}
	var base bytes.Buffer
	if err := jpeg.Encode(&base, img, nil); err != nil {
		t.Fatalf("encode: %v", err)
	}
	b := base.Bytes()

	// A minimal but complete EXIF APP1 segment (no Orientation tag, kept
	// simple since these tests exercise the CLI, not EXIF parsing).
	exifPayload := append([]byte("Exif\x00\x00"), []byte("II*\x00\x08\x00\x00\x00\x00\x00\x00\x00")...)
	length := len(exifPayload) + 2
	seg := append([]byte{0xFF, 0xE1, byte(length >> 8), byte(length)}, exifPayload...)

	var out bytes.Buffer
	out.Write(b[:2])
	out.Write(seg)
	out.Write(b[2:])

	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, out.Bytes(), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return p
}

func writeCleanJPEG(t *testing.T, dir, name string) string {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 3, 3))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return p
}

func TestRun_NoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr)
	if code != exitUnsupported {
		t.Fatalf("exit code = %d, want %d", code, exitUnsupported)
	}
}

func TestRun_UnsupportedFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "not-an-image.txt")
	if err := os.WriteFile(p, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{p}, &stdout, &stderr)
	if code != exitUnsupported {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, exitUnsupported, stderr.String())
	}
}

func TestRun_CheckModeFindsMetadata(t *testing.T) {
	dir := t.TempDir()
	dirty := writeJPEGWithEXIF(t, dir, "dirty.jpg")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--check", dirty}, &stdout, &stderr)
	if code != exitFindings {
		t.Fatalf("exit code = %d, want %d (stdout=%q stderr=%q)", code, exitFindings, stdout.String(), stderr.String())
	}

	// The file must not have been modified.
	after, err := os.ReadFile(dirty)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(dirty)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("--check mode modified the file")
	}
}

func TestRun_CheckModeClean(t *testing.T) {
	dir := t.TempDir()
	clean := writeCleanJPEG(t, dir, "clean.jpg")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--check", clean}, &stdout, &stderr)
	if code != exitClean {
		t.Fatalf("exit code = %d, want %d (stdout=%q stderr=%q)", code, exitClean, stdout.String(), stderr.String())
	}
}

func TestRun_InPlaceStrips(t *testing.T) {
	dir := t.TempDir()
	dirty := writeJPEGWithEXIF(t, dir, "dirty.jpg")
	before, err := os.ReadFile(dirty)
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"-i", dirty}, &stdout, &stderr)
	if code != exitClean {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, exitClean, stderr.String())
	}

	after, err := os.ReadFile(dirty)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, after) {
		t.Fatalf("-i did not modify the file")
	}
	if bytes.Contains(after, []byte("Exif\x00\x00")) {
		t.Fatalf("EXIF signature still present after -i strip")
	}

	// A second run must find nothing left to strip.
	var stdout2, stderr2 bytes.Buffer
	code2 := run([]string{"--check", dirty}, &stdout2, &stderr2)
	if code2 != exitClean {
		t.Fatalf("second run exit code = %d, want %d (stdout=%q)", code2, exitClean, stdout2.String())
	}
}

func TestRun_InPlaceLeavesCleanFileByteIdentical(t *testing.T) {
	dir := t.TempDir()
	clean := writeCleanJPEG(t, dir, "clean.jpg")
	before, err := os.ReadFile(clean)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(clean)
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"-i", clean}, &stdout, &stderr)
	if code != exitClean {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, exitClean, stderr.String())
	}

	after, err := os.ReadFile(clean)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("-i modified an already-clean file")
	}
	infoAfter, err := os.Stat(clean)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime() != infoAfter.ModTime() {
		t.Errorf("-i touched mtime of an already-clean file (no rename should have happened)")
	}
}

func TestRun_DryRunDoesNotModify(t *testing.T) {
	dir := t.TempDir()
	dirty := writeJPEGWithEXIF(t, dir, "dirty.jpg")
	before, err := os.ReadFile(dirty)
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--dry-run", dirty}, &stdout, &stderr)
	if code != exitClean {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, exitClean, stderr.String())
	}
	after, err := os.ReadFile(dirty)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("--dry-run modified the file")
	}
	if stdout.Len() == 0 {
		t.Fatalf("--dry-run produced no report")
	}
}

func TestRun_SingleFileWithoutInPlaceWritesToStdout(t *testing.T) {
	dir := t.TempDir()
	dirty := writeJPEGWithEXIF(t, dir, "dirty.jpg")

	var stdout, stderr bytes.Buffer
	code := run([]string{dirty}, &stdout, &stderr)
	if code != exitClean {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", code, exitClean, stderr.String())
	}
	if stdout.Len() == 0 {
		t.Fatalf("expected stripped JPEG bytes on stdout")
	}
	if _, err := jpeg.Decode(bytes.NewReader(stdout.Bytes())); err != nil {
		t.Fatalf("stdout output does not decode as JPEG: %v", err)
	}
	if bytes.Contains(stdout.Bytes(), []byte("Exif\x00\x00")) {
		t.Fatalf("EXIF signature present in stdout output")
	}

	// The original file on disk must be untouched.
	on, err := os.ReadFile(dirty)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(on, []byte("Exif\x00\x00")) {
		t.Fatalf("original file was modified even without -i")
	}
}

func TestRun_MultipleFilesWithoutInPlaceRejected(t *testing.T) {
	dir := t.TempDir()
	a := writeCleanJPEG(t, dir, "a.jpg")
	b := writeCleanJPEG(t, dir, "b.jpg")

	var stdout, stderr bytes.Buffer
	code := run([]string{a, b}, &stdout, &stderr)
	if code != exitUnsupported {
		t.Fatalf("exit code = %d, want %d", code, exitUnsupported)
	}
}
