// Command privastrip strips privacy-sensitive metadata (EXIF, XMP, IPTC,
// comments) from JPEG, PNG, and WebP files without touching pixel data.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	privastrip "github.com/quietpelican42/privastrip"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// exit codes
const (
	exitClean       = 0
	exitFindings    = 1
	exitUnsupported = 2
)

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("privastrip", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		inPlace         bool
		dryRun          bool
		check           bool
		stripICC        bool
		keepOrientation bool
		quiet           bool
	)
	fs.BoolVar(&inPlace, "in-place", false, "write stripped output back to each file")
	fs.BoolVar(&inPlace, "i", false, "shorthand for -in-place")
	fs.BoolVar(&dryRun, "dry-run", false, "report what would be removed without writing anything")
	fs.BoolVar(&check, "check", false, "exit 1 if any file has metadata; modify nothing")
	fs.BoolVar(&stripICC, "strip-icc", false, "also remove JPEG ICC color profiles (APP2)")
	fs.BoolVar(&keepOrientation, "keep-orientation", true, "preserve JPEG EXIF Orientation via a minimal synthetic tag")
	fs.BoolVar(&quiet, "quiet", false, "suppress the per-file report line")
	fs.Usage = func() {
		fmt.Fprintf(stderr, `Usage: privastrip [flags] file...

Strips EXIF, XMP, IPTC, comments (and optionally ICC profiles) from JPEG,
PNG, and WebP files without decoding or re-encoding pixel data.

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprintf(stderr, `
Exit codes:
  0  no files had metadata, or all metadata was stripped successfully
  1  --check mode found metadata (nothing was modified)
  2  an input was corrupt or not a supported format (JPEG/PNG/WebP)
`)
	}
	if err := fs.Parse(args); err != nil {
		return exitUnsupported
	}

	files := fs.Args()
	if len(files) == 0 {
		fs.Usage()
		return exitUnsupported
	}
	if !inPlace && !check && !dryRun && len(files) > 1 {
		fmt.Fprintln(stderr, "privastrip: without -i, only one file may be given (stripped output is written to stdout)")
		return exitUnsupported
	}

	opts := []privastrip.Option{
		privastrip.WithStripICC(stripICC),
		privastrip.WithKeepOrientation(keepOrientation),
	}

	foundAny := false
	exit := exitClean

	for _, path := range files {
		switch {
		case check:
			rep, err := detectFile(path)
			if err != nil {
				fmt.Fprintf(stderr, "privastrip: %s: %v\n", path, err)
				return exitUnsupported
			}
			if rep.HasMetadata() {
				foundAny = true
				if !quiet {
					printReport(stdout, path, rep, false)
				}
			} else if !quiet {
				fmt.Fprintf(stdout, "%s: clean\n", path)
			}

		case dryRun:
			rep, err := detectFile(path)
			if err != nil {
				fmt.Fprintf(stderr, "privastrip: %s: %v\n", path, err)
				return exitUnsupported
			}
			if !quiet {
				printReport(stdout, path, rep, true)
			}

		case inPlace:
			rep, err := stripFileInPlace(path, opts)
			if err != nil {
				fmt.Fprintf(stderr, "privastrip: %s: %v\n", path, err)
				return exitUnsupported
			}
			if !quiet {
				printReport(stdout, path, rep, false)
			}

		default:
			// Single file, output to stdout.
			rep, err := stripFileToWriter(path, stdout, opts)
			if err != nil {
				fmt.Fprintf(stderr, "privastrip: %s: %v\n", path, err)
				return exitUnsupported
			}
			if !quiet {
				fmt.Fprintf(stderr, "%s\n", reportLine(path, rep, false))
			}
		}
	}

	if check && foundAny {
		exit = exitFindings
	}
	return exit
}

func detectFile(path string) (privastrip.Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return privastrip.Report{}, err
	}
	defer f.Close()
	rep, err := privastrip.Detect(f)
	if err != nil {
		return rep, err
	}
	return rep, nil
}

func stripFileToWriter(path string, w io.Writer, opts []privastrip.Option) (privastrip.Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return privastrip.Report{}, err
	}
	defer f.Close()
	return privastrip.Strip(f, w, opts...)
}

// stripFileInPlace strips path and, if the output differs from the input,
// replaces it atomically via a temp file in the same directory followed by
// a rename.
func stripFileInPlace(path string, opts []privastrip.Option) (privastrip.Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return privastrip.Report{}, err
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".privastrip-*")
	if err != nil {
		f.Close()
		return privastrip.Report{}, err
	}
	tmpPath := tmp.Name()
	cleanupTmp := func() {
		tmp.Close()
		os.Remove(tmpPath)
	}

	rep, err := privastrip.Strip(f, tmp, opts...)
	f.Close()
	if err != nil {
		cleanupTmp()
		return rep, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return rep, err
	}

	if !rep.HasMetadata() {
		os.Remove(tmpPath)
		return rep, nil
	}

	info, statErr := os.Stat(path)
	if statErr == nil {
		_ = os.Chmod(tmpPath, info.Mode())
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return rep, err
	}
	return rep, nil
}

func printReport(w io.Writer, path string, rep privastrip.Report, wouldStrip bool) {
	fmt.Fprintln(w, reportLine(path, rep, wouldStrip))
}

func reportLine(path string, rep privastrip.Report, wouldStrip bool) string {
	if !rep.HasMetadata() {
		return fmt.Sprintf("%s: clean", path)
	}
	verb := "removed"
	if wouldStrip {
		verb = "would remove"
	}
	s := fmt.Sprintf("%s: %s ", path, verb)
	for i, item := range rep.Removed {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%s(%s, %d bytes)", item.Kind, item.Marker, item.Bytes)
	}
	if rep.OrientationPreserved {
		s += " [orientation preserved]"
	}
	return s
}
