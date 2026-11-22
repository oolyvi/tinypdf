package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"pdf_compress/internal/compress"
	"pdf_compress/internal/server"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the local /healthz endpoint and exit (used by Docker HEALTHCHECK)")
	flag.Parse()

	port := env("PORT", "8080")
	if *healthcheck {
		os.Exit(probe(port))
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	gsBin, err := findGhostscript()
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}

	cfg := server.Config{
		MaxUploadBytes: envInt("MAX_UPLOAD_MB", 0) << 20, // 0 = unlimited
		MaxConcurrent:  int(envInt("MAX_CONCURRENT_JOBS", int64(runtime.NumCPU()))),
		JobTimeout:     time.Duration(envInt("JOB_TIMEOUT_SECONDS", 300)) * time.Second,
	}
	srv := server.New(cfg, &compress.Compressor{GSBin: gsBin}, log)

	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      cfg.JobTimeout + 15*time.Minute,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("listening", "addr", httpServer.Addr, "ghostscript", gsBin,
			"max_upload_mb(0=unlimited)", cfg.MaxUploadBytes>>20, "max_concurrent", cfg.MaxConcurrent)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.JobTimeout+10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "err", err)
		os.Exit(1)
	}
}

func findGhostscript() (string, error) {
	candidates := []string{"gs", "gswin64c", "gswin32c"}
	if v := os.Getenv("GS_BIN"); v != "" {
		candidates = []string{v}
	}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
	}
	return "", errors.New("ghostscript not found: install it (e.g. `apt install ghostscript`, `brew install ghostscript`) or set GS_BIN")
}

func probe(port string) int {
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int64) int64 {
	if v, err := strconv.ParseInt(os.Getenv(key), 10, 64); err == nil && v >= 0 {
		return v
	}
	return fallback
}
