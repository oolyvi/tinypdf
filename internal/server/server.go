package server

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"

	"pdf_compress/internal/compress"
)

//go:embed static/index.html
var indexHTML []byte

const (
	defaultReduction = 50.0
	minReduction     = 1.0
	maxReduction     = 90.0
	queueWait        = 30 * time.Second
)

type Config struct {
	MaxUploadBytes int64
	MaxConcurrent  int
	JobTimeout     time.Duration
}

type Server struct {
	cfg        Config
	compressor *compress.Compressor
	slots      chan struct{}
	log        *slog.Logger
}

func New(cfg Config, c *compress.Compressor, log *slog.Logger) *Server {
	return &Server{
		cfg:        cfg,
		compressor: c,
		slots:      make(chan struct{}, max(cfg.MaxConcurrent, 1)),
		log:        log,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("POST /compress", s.handleCompress)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleCompress(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-time.After(queueWait):
		w.Header().Set("Retry-After", "30")
		http.Error(w, "Server is busy, please try again shortly", http.StatusServiceUnavailable)
		return
	case <-r.Context().Done():
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.JobTimeout)
	defer cancel()

	if s.cfg.MaxUploadBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	}

	tempDir, err := os.MkdirTemp("", "pdfcompress-*")
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "Server storage error", err)
		return
	}
	defer os.RemoveAll(tempDir)

	inPath := filepath.Join(tempDir, "input.pdf")
	origName, origSize, reduction, status, err := s.receiveUpload(r, inPath)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			status = http.StatusRequestEntityTooLarge
		}
		s.fail(w, r, status, uploadErrorMessage(err, s.cfg.MaxUploadBytes), err)
		return
	}

	if ok, err := compress.IsPDF(inPath); err != nil || !ok {
		if err == nil {
			err = compress.ErrNotPDF
		}
		s.fail(w, r, http.StatusUnprocessableEntity, compress.ErrNotPDF.Error(), err)
		return
	}

	targetBytes := int64(float64(origSize) * (1 - reduction/100))
	best, err := s.compressor.ToTarget(ctx, tempDir, inPath, origSize, targetBytes)
	if err != nil {
		code := http.StatusUnprocessableEntity // Ghostscript rejecting the file
		msg := "Could not compress this PDF (it may be corrupted or encrypted)"
		if errors.Is(err, context.DeadlineExceeded) {
			code, msg = http.StatusGatewayTimeout, "Compression took too long"
		} else if errors.Is(err, context.Canceled) {
			return // client went away
		}
		s.fail(w, r, code, msg, err)
		return
	}

	best = s.losslessOptimize(ctx, best, filepath.Join(tempDir, "final.pdf"))

	f, err := os.Open(best)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "Failed to read compressed file", err)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "Failed to read compressed file", err)
		return
	}

	actual := 0.0
	if origSize > 0 {
		actual = (1 - float64(stat.Size())/float64(origSize)) * 100
	}

	h := w.Header()
	h.Set("Content-Type", "application/pdf")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "compressed_" + origName}))
	h.Set("X-Compression-Reduction", strconv.FormatFloat(actual, 'f', 1, 64))
	h.Set("X-Target-Met", strconv.FormatBool(actual >= reduction))
	h.Set("X-Original-Size", strconv.FormatInt(origSize, 10))
	h.Set("X-Compressed-Size", strconv.FormatInt(stat.Size(), 10))

	s.log.Info("compressed",
		"file", origName, "orig_bytes", origSize, "out_bytes", stat.Size(),
		"target_pct", reduction, "actual_pct", fmt.Sprintf("%.1f", actual),
		"took", time.Since(start).Round(time.Millisecond))

	http.ServeContent(w, r, origName, stat.ModTime(), f)
}

func (s *Server) receiveUpload(r *http.Request, inPath string) (name string, size int64, reduction float64, status int, err error) {
	reduction = defaultReduction

	mr, err := r.MultipartReader()
	if err != nil {
		return "", 0, 0, http.StatusBadRequest, errors.New("invalid multipart request")
	}

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", 0, 0, http.StatusBadRequest, err
		}

		switch part.FormName() {
		case "target_reduction":
			buf, _ := io.ReadAll(io.LimitReader(part, 16))
			if v, perr := strconv.ParseFloat(strings.TrimSpace(string(buf)), 64); perr == nil {
				reduction = min(max(v, minReduction), maxReduction)
			}

		case "pdf":
			if part.FileName() == "" {
				continue
			}
			name = sanitizeFilename(part.FileName())

			dst, err := os.Create(inPath)
			if err != nil {
				return "", 0, 0, http.StatusInternalServerError, err
			}
			n, copyErr := io.Copy(dst, part)
			closeErr := dst.Close()
			if copyErr != nil {
				return "", 0, 0, http.StatusBadRequest, copyErr
			}
			if closeErr != nil {
				return "", 0, 0, http.StatusInternalServerError, closeErr
			}
			size = n
		}
	}

	if name == "" || size == 0 {
		return "", 0, 0, http.StatusBadRequest, errors.New("no PDF file provided")
	}
	return name, size, reduction, 0, nil
}

func (s *Server) losslessOptimize(ctx context.Context, in, out string) string {
	conf, err := api.LoadConfiguration(api.ConfigurationOptions{Mode: api.ConfigurationModeStateless})
	if err != nil {
		return in
	}
	if err := api.OptimizeFile(ctx, in, out, conf, nil); err != nil {
		s.log.Debug("pdfcpu optimize skipped", "err", err)
		return in
	}
	inInfo, err1 := os.Stat(in)
	outInfo, err2 := os.Stat(out)
	if err1 == nil && err2 == nil && outInfo.Size() < inInfo.Size() {
		return out
	}
	return in
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, code int, msg string, err error) {
	s.log.Warn("request failed", "path", r.URL.Path, "status", code, "err", err)
	http.Error(w, msg, code)
}

func uploadErrorMessage(err error, limit int64) string {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return fmt.Sprintf("File too large (limit is %d MB)", limit>>20)
	}
	return err.Error()
}

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == "/" {
		return "document.pdf"
	}
	return name
}
