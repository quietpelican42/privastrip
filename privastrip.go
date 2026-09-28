// Package privastrip removes privacy-sensitive metadata from image files
// without decoding or re-encoding pixel data.
//
// It operates at the container level: JPEG segments, PNG chunks, and WebP
// RIFF chunks are parsed, classified, and either copied through unchanged or
// dropped. Pixel data is never touched, so a stripped file decodes to
// exactly the same image as the original.
package privastrip

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// Format identifies the container format of an image.
type Format int

// Supported and recognized-but-unsupported formats.
const (
	FormatUnknown Format = iota
	FormatJPEG
	FormatPNG
	FormatWebP
)

// String returns a human-readable name for the format.
func (f Format) String() string {
	switch f {
	case FormatJPEG:
		return "JPEG"
	case FormatPNG:
		return "PNG"
	case FormatWebP:
		return "WebP"
	default:
		return "unknown"
	}
}

// Kind identifies the category of a removed (or detected) metadata block.
type Kind string

// Metadata kinds privastrip recognizes across formats.
const (
	KindEXIF        Kind = "EXIF"
	KindXMP         Kind = "XMP"
	KindXMPExtended Kind = "XMP-Extended"
	KindIPTC        Kind = "IPTC"
	KindICC         Kind = "ICC"
	KindComment     Kind = "Comment"
	KindText        Kind = "Text"
	KindTimestamp   Kind = "Timestamp"
	KindUnknownAPPn Kind = "APPn"
)

// Item describes one metadata block that was found (Detect) or removed (Strip).
type Item struct {
	// Kind categorizes the metadata (EXIF, XMP, IPTC, ICC, ...).
	Kind Kind
	// Marker or chunk type as it appears in the container, e.g. "APP1" or "tEXt".
	Marker string
	// Bytes is the size of the block including its header (marker/length or
	// chunk type/length/CRC), i.e. what leaves the file when it is removed.
	Bytes int
}

// Report describes what privastrip found or removed from one image.
type Report struct {
	// Format is the detected container format.
	Format Format
	// Removed lists every metadata block that was (or, for Detect, would be)
	// removed, in file order.
	Removed []Item
	// OrientationPreserved is true when a JPEG EXIF Orientation tag was kept
	// by rewriting a minimal synthetic EXIF block in place of the original.
	OrientationPreserved bool
	// BytesIn is the number of input bytes read.
	BytesIn int64
	// BytesOut is the number of output bytes written. For Detect, this
	// equals BytesIn since nothing is written.
	BytesOut int64
}

// HasMetadata reports whether any metadata was found.
func (r Report) HasMetadata() bool {
	return len(r.Removed) > 0
}

// ErrUnsupportedFormat is returned when the input is not a recognized JPEG,
// PNG, or WebP file.
var ErrUnsupportedFormat = errors.New("privastrip: unsupported or unrecognized image format")

// ErrCorrupt is returned when the input looks like a supported format but is
// truncated or violates its container structure.
var ErrCorrupt = errors.New("privastrip: malformed input")

// Options configures Strip and Detect.
type Options struct {
	stripICC        bool
	keepOrientation bool
}

// Option configures Options.
type Option func(*Options)

// WithStripICC controls whether JPEG APP2 ICC color profiles are removed.
// ICC profiles carry no personal information but do affect color rendering;
// the default is to keep them. Pass true to remove them too.
func WithStripICC(strip bool) Option {
	return func(o *Options) { o.stripICC = strip }
}

// WithKeepOrientation controls whether a JPEG's EXIF Orientation tag is
// preserved by rewriting a minimal synthetic EXIF block after the original
// (full) EXIF block is removed. Default true. See the package README for the
// trade-off: with this off, an image taken in a rotated position may display
// sideways or upside down after stripping, because the pixel data itself is
// never rotated — only the EXIF tag told viewers how to rotate it.
func WithKeepOrientation(keep bool) Option {
	return func(o *Options) { o.keepOrientation = keep }
}

func newOptions(opts []Option) Options {
	o := Options{keepOrientation: true}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// sniffSize is the number of leading bytes needed to identify any supported format.
const sniffSize = 12

// DetectFormat reads the leading bytes of r (via a small peek) and returns
// the container format, or FormatUnknown if it is not recognized.
func DetectFormat(r *bufio.Reader) (Format, error) {
	head, err := r.Peek(sniffSize)
	if err != nil && err != io.EOF {
		return FormatUnknown, err
	}
	return sniffFormat(head), nil
}

func sniffFormat(head []byte) Format {
	switch {
	case len(head) >= 2 && head[0] == 0xFF && head[1] == 0xD8:
		return FormatJPEG
	case len(head) >= 8 && string(head[:8]) == "\x89PNG\r\n\x1a\n":
		return FormatPNG
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return FormatWebP
	default:
		return FormatUnknown
	}
}

// Strip copies r to w, removing privacy-sensitive metadata (EXIF, XMP, IPTC,
// comments, and optionally ICC profiles) while leaving pixel data byte-for-byte
// reproducible. It returns a Report of what was removed.
//
// Strip supports JPEG, PNG, and WebP. Any other input returns
// ErrUnsupportedFormat and writes nothing. A structurally invalid file of a
// supported format returns ErrCorrupt, possibly after partial output has
// already been written to w.
func Strip(r io.Reader, w io.Writer, opts ...Option) (Report, error) {
	o := newOptions(opts)
	cr := &countingReader{r: r}
	br := bufio.NewReaderSize(cr, 4096)
	format, err := DetectFormat(br)
	if err != nil {
		return Report{}, fmt.Errorf("privastrip: reading input: %w", err)
	}

	cw := &countingWriter{w: w}
	var rep Report
	switch format {
	case FormatJPEG:
		rep, err = stripJPEG(br, cw, o)
	case FormatPNG:
		rep, err = stripPNG(br, cw)
	case FormatWebP:
		rep, err = stripWebP(br, cw)
	default:
		return Report{Format: FormatUnknown}, ErrUnsupportedFormat
	}
	rep.Format = format
	rep.BytesIn = cr.n
	rep.BytesOut = cw.n
	return rep, err
}

// Detect parses r and reports what metadata is present without writing
// anything. It supports the same formats as Strip and returns
// ErrUnsupportedFormat / ErrCorrupt under the same conditions.
func Detect(r io.Reader) (Report, error) {
	cr := &countingReader{r: r}
	br := bufio.NewReaderSize(cr, 4096)
	format, err := DetectFormat(br)
	if err != nil {
		return Report{}, fmt.Errorf("privastrip: reading input: %w", err)
	}

	var rep Report
	switch format {
	case FormatJPEG:
		rep, err = stripJPEG(br, nil, newOptions(nil))
	case FormatPNG:
		rep, err = stripPNG(br, nil)
	case FormatWebP:
		rep, err = stripWebP(br, nil)
	default:
		return Report{Format: FormatUnknown}, ErrUnsupportedFormat
	}
	rep.Format = format
	rep.BytesIn = cr.n
	rep.BytesOut = rep.BytesIn
	return rep, err
}

// countingWriter wraps an io.Writer (possibly nil, for dry-run/Detect mode)
// and counts bytes written.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	if c.w == nil {
		return len(p), nil
	}
	return c.w.Write(p)
}

// countingReader wraps an io.Reader and counts bytes read from it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// discardOrCopy copies n bytes from r to w. If w is nil, it discards them
// (used in Detect / dry-run mode) while still using a bounded internal
// buffer regardless of n.
func discardOrCopy(w io.Writer, r io.Reader, n int64) (int64, error) {
	if w == nil {
		return io.CopyN(io.Discard, r, n)
	}
	return io.CopyN(w, r, n)
}

// writeAll writes p to w, unless w is nil (dry-run / Detect mode) in which
// case it is a no-op.
func writeAll(w io.Writer, p []byte) error {
	if w == nil {
		return nil
	}
	_, err := w.Write(p)
	return err
}
