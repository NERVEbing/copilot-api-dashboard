package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/NERVEbing/copilot-api-dashboard/internal/config"
	"github.com/NERVEbing/copilot-api-dashboard/internal/dashboard"
	"github.com/NERVEbing/copilot-api-dashboard/internal/discovery"
	"github.com/NERVEbing/copilot-api-dashboard/internal/httpserver"
	"github.com/NERVEbing/copilot-api-dashboard/internal/upstream"
	"github.com/moby/moby/client"
)

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})))
	docker, err := client.New(client.WithHost("unix://" + discovery.SocketPath))
	if err != nil {
		return errors.New("cannot initialize Docker client")
	}
	defer docker.Close()
	up := upstream.New(cfg.RequestTimeout, cfg.MaxConcurrency)
	defer up.Close()
	service := &dashboard.Service{Discovery: &discovery.Discoverer{File: cfg.EndpointsFile, Image: cfg.DockerImage, Timeout: cfg.RequestTimeout, Docker: docker}, Upstream: up}
	server := &http.Server{Addr: cfg.ListenAddr, Handler: httpserver.New(service), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	slog.Info("Dashboard listening", "address", cfg.ListenAddr)
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("HTTP server failed to listen or serve")
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return errors.New("HTTP shutdown timed out")
		}
		return nil
	}
}

func main() {
	if err := run(); err != nil {
		slog.Error("Dashboard stopped", "error", err)
		os.Exit(1)
	}
}
