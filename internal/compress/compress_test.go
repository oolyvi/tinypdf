package compress

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLevelParams(t *testing.T) {
	tests := []struct {
		level, dpi, q int
	}{
		{-10, 300, 95},
		{0, 300, 95},
		{50, 175, 58},
		{100, 50, 20},
		{500, 50, 20},
	}
	for _, tc := range tests {
		dpi, q := LevelParams(tc.level)
		if dpi != tc.dpi || q != tc.q {
			t.Errorf("LevelParams(%d) = (%d, %d), want (%d, %d)", tc.level, dpi, q, tc.dpi, tc.q)
		}
	}
}

func TestIsPDF(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.pdf")
	bad := filepath.Join(dir, "bad.pdf")
	_ = os.WriteFile(good, []byte("%PDF-1.7\n..."), 0o600)
	_ = os.WriteFile(bad, []byte("hello world"), 0o600)

	if ok, err := IsPDF(good); err != nil || !ok {
		t.Errorf("IsPDF(good) = %v, %v; want true, nil", ok, err)
	}
	if ok, err := IsPDF(bad); err != nil || ok {
		t.Errorf("IsPDF(bad) = %v, %v; want false, nil", ok, err)
	}
	if _, err := IsPDF(filepath.Join(dir, "missing.pdf")); err == nil {
		t.Error("IsPDF(missing) expected an error")
	}
}

func TestToTargetEndToEnd(t *testing.T) {
	gs, err := exec.LookPath("gs")
	if err != nil {
		t.Skip("ghostscript not installed")
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "in.pdf")
	if out, err := exec.Command(gs, "-sDEVICE=pdfwrite", "-dBATCH", "-dNOPAUSE", "-dQUIET",
		"-sOutputFile="+in, "-c", "/Helvetica findfont 24 scalefont setfont 72 700 moveto (hi) show showpage").CombinedOutput(); err != nil {
		t.Fatalf("generating fixture: %v: %s", err, out)
	}
	info, _ := os.Stat(in)

	c := &Compressor{GSBin: gs}
	got, err := c.ToTarget(context.Background(), dir, in, info.Size(), info.Size()/2)
	if err != nil {
		t.Fatalf("ToTarget: %v", err)
	}
	if ok, _ := IsPDF(got); !ok {
		t.Errorf("result %s is not a PDF", got)
	}
}
