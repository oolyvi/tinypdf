package compress

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

var ErrNotPDF = errors.New("file is not a valid PDF")

const (
	searchSteps = 4
	tolerance   = 0.10
)

type Compressor struct {
	GSBin string
}

func IsPDF(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	buf := make([]byte, 1024)
	n, _ := f.Read(buf)
	return bytes.Contains(buf[:n], []byte("%PDF-")), nil
}

func LevelParams(level int) (dpi, jpegQuality int) {
	level = min(max(level, 0), 100)
	dpi = 300 - (level * 250 / 100)
	jpegQuality = 95 - (level * 75 / 100)
	return dpi, jpegQuality
}

func (c *Compressor) run(ctx context.Context, in, out string, dpi, jpegQuality int) error {
	args := []string{
		"-sDEVICE=pdfwrite",
		"-dCompatibilityLevel=1.5",
		"-dSAFER",
		"-dNOPAUSE",
		"-dQUIET",
		"-dBATCH",
		"-dDetectDuplicateImages=true",
		"-dCompressFonts=true",
		"-dSubsetFonts=true",
		"-dDownsampleColorImages=true",
		"-dColorImageDownsampleType=/Bicubic",
		fmt.Sprintf("-dColorImageResolution=%d", dpi),
		"-dDownsampleGrayImages=true",
		"-dGrayImageDownsampleType=/Bicubic",
		fmt.Sprintf("-dGrayImageResolution=%d", dpi),
		"-dDownsampleMonoImages=true",
		"-dMonoImageDownsampleType=/Subsample",
		fmt.Sprintf("-dMonoImageResolution=%d", dpi*2),
		"-dAutoFilterColorImages=false",
		"-dColorImageFilter=/DCTEncode",
		fmt.Sprintf("-dJPEGQ=%d", jpegQuality),
		"-sOutputFile=" + out,
		in,
	}

	output, err := exec.CommandContext(ctx, c.GSBin, args...).CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("ghostscript: %w: %s", err, bytes.TrimSpace(output))
	}
	return nil
}

func (c *Compressor) ToTarget(ctx context.Context, dir, in string, origSize, targetBytes int64) (string, error) {
	var (
		under, over         string
		underSize, overSize int64
		lastPass            time.Duration
	)
	drop := func(p string) {
		if p != "" {
			_ = os.Remove(p)
		}
	}

	low, high := 0, 100
	for step := 0; step < searchSteps && low <= high; step++ {
		if dl, ok := ctx.Deadline(); ok && step > 0 {
			if time.Until(dl) < lastPass*3/2 && (under != "" || over != "") {
				break
			}
		}

		mid := (low + high) / 2
		dpi, q := LevelParams(mid)

		candidate := filepath.Join(dir, fmt.Sprintf("pass_%d.pdf", step))
		t0 := time.Now()
		if err := c.run(ctx, in, candidate, dpi, q); err != nil {
			if ctx.Err() != nil && (under != "" || over != "") {
				_ = os.Remove(candidate)
				break
			}
			return "", err
		}
		lastPass = time.Since(t0)

		info, err := os.Stat(candidate)
		if err != nil {
			return "", err
		}
		size := info.Size()

		if size <= targetBytes {
			if under == "" || size > underSize {
				drop(under)
				under, underSize = candidate, size
			} else {
				drop(candidate)
			}
			if float64(targetBytes-size) <= float64(origSize)*tolerance {
				break
			}
			high = mid - 1
		} else {
			if over == "" || size < overSize {
				drop(over)
				over, overSize = candidate, size
			} else {
				drop(candidate)
			}
			low = mid + 1
		}
	}

	switch {
	case under != "":
		drop(over)
		return under, nil
	case over != "" && overSize < origSize:
		return over, nil
	default:
		drop(over)
		return in, nil
	}
}
