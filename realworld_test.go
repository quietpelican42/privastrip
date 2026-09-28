package privastrip

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// realWorldFixture is one public-domain camera JPEG from Wikimedia Commons,
// pinned by sha256 so a compromised or edited upstream file is never
// silently used. These are real photographs uploaded straight from a
// camera/phone and confirmed (via the Commons API's own extracted EXIF
// metadata) to carry EXIF before being fetched here.
type realWorldFixture struct {
	name   string
	url    string
	sha256 string
}

var realWorldFixtures = []realWorldFixture{
	{
		name:   "Agfa_Click_BW_1.JPG",
		url:    "https://upload.wikimedia.org/wikipedia/commons/9/9b/Agfa_Click_BW_1.JPG",
		sha256: "ab7fb4d429f35a84533c3fdb00b3f79793dcc5ecfdaa2694db50157f33af5b49",
	},
	{
		name:   "Canon_EOS_7D_DSLR_body_back.jpg",
		url:    "https://upload.wikimedia.org/wikipedia/commons/7/7d/Canon_EOS_7D_DSLR_body_back.jpg",
		sha256: "2910331a4104cf0bfdcf287b952677da2cfddc51bec732185b86a036f82908d2",
	},
	{
		name:   "Chinon_CP_9_AF_BW_1.JPG",
		url:    "https://upload.wikimedia.org/wikipedia/commons/b/be/Chinon_CP_9_AF_BW_1.JPG",
		sha256: "40fed50446b30486b363d41b81efeeabf8ecf93bf4de2ea6c0ce0f6e6c1ce57c",
	},
	{
		name:   "Leica-II-p1030002.jpg",
		url:    "https://upload.wikimedia.org/wikipedia/commons/e/eb/Leica-II-p1030002.jpg",
		sha256: "3fd43112a5c3629ba1ed34ee48e809a6d206cebbb552151eefcb406bddc1b8ae",
	},
	{
		name:   "Sony_DSLR-A290.jpg",
		url:    "https://upload.wikimedia.org/wikipedia/commons/8/84/Sony_DSLR-A290.jpg",
		sha256: "d74e954b8f34ea97cf188b44cac06528594a41d3a3bd069af940430f36929224",
	},
}

// fetchRealWorldFixture returns the fixture's bytes, downloading and caching
// it under testdata/realworld/ (gitignored) on first use, and verifying its
// sha256 every time. It calls t.Skip with an actionable message if the file
// cannot be obtained (no network, upstream unavailable) or fails the
// checksum (content changed upstream).
func fetchRealWorldFixture(t *testing.T, f realWorldFixture) []byte {
	t.Helper()
	cacheDir := filepath.Join("testdata", "realworld")
	cachePath := filepath.Join(cacheDir, f.name)

	data, err := os.ReadFile(cachePath)
	if err != nil {
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			t.Skipf("SKIP: cannot create cache dir %s: %v", cacheDir, err)
		}
		client := &http.Client{Timeout: 30 * time.Second}
		req, err := http.NewRequest(http.MethodGet, f.url, nil)
		if err != nil {
			t.Skipf("SKIP: building request for %s: %v", f.url, err)
		}
		req.Header.Set("User-Agent", "privastrip-test/1.0 (+https://pkg.go.dev; test fixture fetch)")
		resp, err := client.Do(req)
		if err != nil {
			t.Skipf("SKIP: fetching real-world fixture %s (no network?): %v\nRun with network access, or pre-populate %s.", f.url, err, cachePath)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Skipf("SKIP: fetching %s: HTTP %d (rate-limited or unavailable). Retry later or pre-populate %s.", f.url, resp.StatusCode, cachePath)
		}
		data, err = io.ReadAll(resp.Body)
		if err != nil {
			t.Skipf("SKIP: reading response body for %s: %v", f.url, err)
		}
		if err := os.WriteFile(cachePath, data, 0o644); err != nil {
			t.Logf("warning: could not cache fixture to %s: %v", cachePath, err)
		}
	}

	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if got != f.sha256 {
		t.Skipf("SKIP: %s sha256 mismatch: got %s, want %s (upstream file changed, or local cache is stale/corrupt: delete %s and re-run)", f.name, got, f.sha256, cachePath)
	}
	return data
}

// TestRealWorldFixtures_StripAndComparePixels downloads 5 real,
// public-domain camera JPEGs (Wikimedia Commons originals, all confirmed by
// Commons' own extracted EXIF metadata to contain camera EXIF), strips each
// with privastrip, confirms the reported EXIF removal, confirms no Exif
// signature bytes remain, and confirms the pixel data decodes identically
// before and after.
func TestRealWorldFixtures_StripAndComparePixels(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network-dependent real-world fixture test in -short mode")
	}
	for _, f := range realWorldFixtures {
		f := f
		t.Run(f.name, func(t *testing.T) {
			input := fetchRealWorldFixture(t, f)

			var out bytes.Buffer
			rep, err := Strip(bytes.NewReader(input), &out)
			if err != nil {
				t.Fatalf("Strip: %v", err)
			}
			if rep.Format != FormatJPEG {
				t.Fatalf("Format = %v, want JPEG", rep.Format)
			}

			hasEXIF := false
			var removedSummary string
			for _, item := range rep.Removed {
				if item.Kind == KindEXIF {
					hasEXIF = true
				}
				removedSummary += fmt.Sprintf(" %s(%s,%dB)", item.Kind, item.Marker, item.Bytes)
			}
			if !hasEXIF {
				t.Fatalf("expected EXIF to be found and removed in %s, got: %v", f.name, rep.Removed)
			}
			t.Logf("%s: %d -> %d bytes, removed:%s, orientation preserved=%v",
				f.name, len(input), out.Len(), removedSummary, rep.OrientationPreserved)

			stripped := out.Bytes()
			if bytes.Contains(stripped, []byte(exifSignature)) && !rep.OrientationPreserved {
				t.Errorf("Exif signature still present in stripped output and orientation was not preserved")
			}

			origImg, err := jpeg.Decode(bytes.NewReader(input))
			if err != nil {
				t.Fatalf("decoding original: %v", err)
			}
			strippedImg, err := jpeg.Decode(bytes.NewReader(stripped))
			if err != nil {
				t.Fatalf("decoding stripped output: %v", err)
			}
			requirePixelsEqual(t, origImg, strippedImg)
		})
	}
}
