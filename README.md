# privastrip

Removes EXIF, XMP, IPTC, and comment metadata from JPEG, PNG, and WebP
files without decoding or re-encoding pixel data.

```go
f, _ := os.Open("photo.jpg")
out, _ := os.Create("photo-stripped.jpg")
report, err := privastrip.Strip(f, out)
// report.Removed lists every EXIF/XMP/IPTC/comment block that came out.
```

## Why

A photo straight off a phone or camera carries EXIF (camera make/model,
timestamp, and often GPS coordinates), and frequently XMP and IPTC blocks
added by editing software (author, location, software used). Sharing the
file as-is leaks all of it.

Go's standard library has no metadata stripper: `image/jpeg` and
`image/png` decode and re-encode pixels, discarding metadata as a side
effect, but re-encoding a JPEG is lossy (it requantizes and re-runs DCT/
Huffman coding) and re-encoding a PNG changes its compressed bytes even
though the pixels are the same. Neither is a "strip metadata, keep
everything else byte-identical" tool.

The one existing dedicated Go package for this,
[go-exif-remove](https://github.com/scottleedavis/go-exif-remove)
(archived, last release 2023), does not work: its `Remove()` returns the
input completely unchanged, silently, no error. Measured directly:

Measured: a small program calling `exifremove.Remove()` from
`github.com/scottleedavis/go-exif-remove@v0.0.0-20230314195146-7e059d593405`
on 5 real camera JPEGs (see Compatibility for provenance), each of which
privastrip confirms contains an EXIF APP1 segment:

```
Agfa_Click_BW_1.JPG: in=1378668 out=1378668 identical=true
Canon_EOS_7D_DSLR_body_back.jpg: in=2123466 out=2123466 identical=true
Chinon_CP_9_AF_BW_1.JPG: in=1807109 out=1807109 identical=true
Leica-II-p1030002.jpg: in=559005 out=559005 identical=true
Sony_DSLR-A290.jpg: in=487930 out=487930 identical=true
```

Every output is byte-identical to its input: the library is a no-op on
real files. (Node has `exif-be-gone`; Python has `mat2`. This gap is
Go-specific.)

privastrip strips the same 5 files for real:

Measured: `go test -run TestRealWorldFixtures -v .`

```
Agfa_Click_BW_1.JPG: 1378668 -> 1327234 bytes, removed EXIF(APP1,51470B), orientation preserved=true
Canon_EOS_7D_DSLR_body_back.jpg: 2123466 -> 2101080 bytes, removed EXIF(APP1,7518B) IPTC(APP13,9502B) XMP(APP1,5402B), orientation preserved=true
Chinon_CP_9_AF_BW_1.JPG: 1807109 -> 1756007 bytes, removed EXIF(APP1,51138B), orientation preserved=true
Leica-II-p1030002.jpg: 559005 -> 545767 bytes, removed EXIF(APP1,3684B) IPTC(APP13,4908B) XMP(APP1,4682B), orientation preserved=true
Sony_DSLR-A290.jpg: 487930 -> 480022 bytes, removed EXIF(APP1,7944B), orientation preserved=true
```

Every file decodes to pixel-identical output before and after (checked by
the same test, via `image/jpeg` decode and per-pixel comparison).

## Installation

```sh
go get github.com/__GH_OWNER__/privastrip
```

Go 1.25 or later. No runtime dependencies. A CLI is included at
`cmd/privastrip`.

## Usage

### Strip a file

```go
f, err := os.Open("photo.jpg")
if err != nil {
    log.Fatal(err)
}
defer f.Close()

out, err := os.Create("photo-stripped.jpg")
if err != nil {
    log.Fatal(err)
}
defer out.Close()

report, err := privastrip.Strip(f, out)
if err != nil {
    log.Fatal(err)
}
for _, item := range report.Removed {
    fmt.Printf("removed %s (%s, %d bytes)\n", item.Kind, item.Marker, item.Bytes)
}
```

### Check for metadata without modifying anything

```go
report, err := privastrip.Detect(f)
if err != nil {
    log.Fatal(err)
}
if report.HasMetadata() {
    // report.Removed lists what's present, without having written anything.
}
```

### Also remove the ICC color profile

ICC profiles carry no personal information but do affect how an image's
colors render. They are kept by default; pass `WithStripICC(true)` to
remove them too (JPEG only — see Limitations).

```go
privastrip.Strip(f, out, privastrip.WithStripICC(true))
```

### Drop EXIF Orientation instead of preserving it

By default, if a dropped EXIF block set the Orientation tag, privastrip
rewrites a minimal synthetic EXIF block containing only that tag, so the
image keeps displaying right-side up. Pass `WithKeepOrientation(false)` to
drop it along with everything else — see Limitations for the trade-off.

```go
privastrip.Strip(f, out, privastrip.WithKeepOrientation(false))
```

### CLI

```sh
privastrip photo.jpg > clean.jpg          # stripped bytes to stdout
privastrip -i photo1.jpg photo2.png       # strip in place
privastrip --check *.jpg                  # exit 1 if any file has metadata
privastrip --dry-run -i photo.jpg         # report what would be removed
```

## Reference

### `Strip(r io.Reader, w io.Writer, opts ...Option) (Report, error)`

Copies `r` to `w`, removing metadata, and returns a `Report` of what was
removed. Supports JPEG, PNG, and WebP (sniffed from the leading bytes).
Any other input returns `ErrUnsupportedFormat` and writes nothing. A
structurally invalid file of a supported format returns `ErrCorrupt`,
possibly after partial output has already reached `w`.

### `Detect(r io.Reader) (Report, error)`

Parses `r` and reports what metadata is present, without writing
anything. Same format support and errors as `Strip`.

### Options

| Option | Default | Effect |
| --- | --- | --- |
| `WithStripICC(bool)` | `false` | JPEG only: also remove the APP2 ICC color profile. |
| `WithKeepOrientation(bool)` | `true` | JPEG only: rewrite a minimal synthetic EXIF Orientation tag after removing the original EXIF block. |

### `Report`

| Field | Meaning |
| --- | --- |
| `Format` | `FormatJPEG`, `FormatPNG`, `FormatWebP`, or `FormatUnknown` |
| `Removed` | `[]Item`, in file order |
| `OrientationPreserved` | true if a synthetic EXIF Orientation tag was written (JPEG) |
| `BytesIn` / `BytesOut` | bytes read / written (equal for `Detect`) |

`Report.HasMetadata()` reports whether `Removed` is non-empty.

### `Item`

`Kind` (`EXIF`, `XMP`, `XMP-Extended`, `IPTC`, `ICC`, `Comment`, `Text`,
`Timestamp`), `Marker` (e.g. `APP1`, `tEXt`, `EXIF`), and `Bytes` (size
removed, including the segment/chunk header).

### CLI flags

| Flag | Meaning |
| --- | --- |
| `-i`, `--in-place` | Write stripped output back to each file (atomic rename via a temp file). |
| `--dry-run` | Report what would be removed; modify nothing. |
| `--check` | Exit 1 if any file has metadata; modify nothing. |
| `--strip-icc` | Also remove JPEG ICC profiles. |
| `--keep-orientation` | Default `true`; set `=false` to drop EXIF Orientation too. |
| `--quiet` | Suppress the per-file report line. |

Without `-i`, `--check`, or `--dry-run`, exactly one file must be given;
its stripped bytes go to stdout.

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | No files had metadata, or all metadata was stripped successfully. |
| 1 | `--check` found metadata in at least one file. Nothing was modified. |
| 2 | An input was corrupt, or not JPEG/PNG/WebP, or usage was wrong. |

## How it works

privastrip never calls `image/jpeg.Decode`, `image/png.Decode`, or
anything that touches pixels. It parses each format's container structure
directly and either passes a segment/chunk through unchanged or drops it:

- **JPEG**: walks marker segments after SOI. Drops APP1 (sniffed as
  `Exif\0\0`, the Adobe XMP URI, or the Extended XMP URI), APP13 (sniffed
  as a `Photoshop 3.0\0` IRB, which is where IPTC lives), and COM. APP2 is
  dropped only when it sniffs as `ICC_PROFILE\0` and `WithStripICC(true)`
  is set. Everything else — APP0/JFIF, APP14 (Adobe color transform,
  needed to decode CMYK/YCCK correctly), DQT/DHT/SOF/DRI, and any
  unrecognized APPn — is kept untouched. Once SOS (start of scan) is
  reached, the rest of the stream (entropy-coded data, any further scans,
  EOI) is copied through byte-for-byte without inspection, since metadata
  never appears there in a conforming file.
- **PNG**: walks chunks after the 8-byte signature. Drops `eXIf`, `tEXt`,
  `zTXt`, `iTXt`, and `tIME`. Everything else — including `iCCP`, `sRGB`,
  `gAMA`, `cHRM`, and animation chunks — is copied through with its
  original CRC, since chunks are independent and removing one never
  invalidates another's CRC.
- **WebP**: walks RIFF chunks after `RIFF`/size/`WEBP`. Drops `EXIF` and
  `XMP ` chunks and clears their two flag bits in the `VP8X` chunk if
  present. Because the RIFF size field precedes the chunks it describes
  and the API takes a forward-only `io.Writer`, the re-serialized body is
  assembled in memory before the header (with its corrected size) is
  written; see Limitations.

Segment/chunk lengths are always read from the container's own bounded
length field (JPEG: 16-bit, so ≤65535 bytes; PNG/WebP: streamed via
`io.CopyN` with a fixed internal buffer regardless of the declared
length), so a crafted length field cannot force an allocation
proportional to an attacker-chosen value.

## Limitations

- JPEG APP1 payloads that don't match the Exif or XMP signatures are left
  alone rather than guessed at; in practice APP1 is used for nothing else.
- APP2 is only recognized (and only stripped with `--strip-icc`) when it
  sniffs as an ICC profile. APP2 also carries Multi-Picture Format (MPF)
  data on some cameras; that is left untouched either way, never removed.
- `WithKeepOrientation(true)` (the default) only preserves the
  Orientation tag itself — nothing else from the EXIF block, and the
  pixel data is never rotated. A viewer that ignores EXIF orientation
  (many raw pixel viewers, some server-side thumbnailers) will display
  the image exactly as before stripping. A viewer that honors it will
  keep rotating it exactly as before. Only `WithKeepOrientation(false)`
  guarantees zero EXIF bytes remain, at the cost of a possibly
  sideways/upside-down display in viewers that relied on the tag.
- WebP ICC profiles are never removed (no `--strip-icc` equivalent for
  WebP); only EXIF and XMP are in scope there.
- WebP stripping buffers the whole re-serialized body in memory to
  recompute the RIFF size before writing the header. Output never exceeds
  input size, so this is bounded by the input file itself, not by
  anything separately attacker-controlled — but it means a very large
  WebP (e.g. a long animation) is held whole in memory rather than
  streamed.
- HEIC/AVIF (ISOBMFF) are not supported: correctly removing an `iinf`/
  `iloc` metadata item without corrupting box offsets elsewhere in the
  file is materially harder than the segment/chunk formats above, and
  guessing wrong there is exactly the kind of lossy mistake this package
  exists to avoid. `Strip`/`Detect` return `ErrUnsupportedFormat` (CLI
  exit 2) for these rather than a wrong answer.
- Metadata embedded in a JPEG comment placed after SOS (nonstandard, but
  seen from some encoders) is not detected: everything after SOS is
  copied through verbatim, by design (see How it works).

## Compatibility

Tested with `go1.27.1 darwin/arm64`. CI runs the same checks on Ubuntu
with Go 1.27 (`.github/workflows/test.yml`); other platforms are
untested.

The real-world fixture test (`TestRealWorldFixtures_StripAndComparePixels`)
downloads 5 public-domain camera JPEGs from Wikimedia Commons at test
time — each confirmed via the Commons API's own extracted EXIF metadata
to contain camera EXIF before download — verifies each against a pinned
sha256, and skips loudly (not a failure) if network access is unavailable
or the checksum doesn't match. It does not run in this repository's CI.

## Development

```sh
go build ./...
go vet ./...
go test -race -count=1 ./...
gofmt -l .                                  # must print nothing
go test -run '^$' -fuzz=FuzzStripJPEG -fuzztime=30s .
go test -run '^$' -fuzz=FuzzStripPNG -fuzztime=30s .
go test -run '^$' -fuzz=FuzzStripWebP -fuzztime=30s .
```

## License

MIT, see [LICENSE](LICENSE).
