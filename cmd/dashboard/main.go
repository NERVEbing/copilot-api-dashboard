package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/moby/moby/client"

	"github.com/NERVEbing/copilot-api-dashboard/internal/config"
	"github.com/NERVEbing/copilot-api-dashboard/internal/dashboard"
	"github.com/NERVEbing/copilot-api-dashboard/internal/discovery"
	"github.com/NERVEbing/copilot-api-dashboard/internal/httpserver"
	sqlitestore "github.com/NERVEbing/copilot-api-dashboard/internal/store/sqlite"
	"github.com/NERVEbing/copilot-api-dashboard/internal/upstream"
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
	defer func() {
		if err := docker.Close(); err != nil {
			slog.Warn("Failed to close Docker client", "error", err)
		}
	}()
	up := upstream.New(cfg.RequestTimeout, cfg.MaxConcurrency)
	defer up.Close()
	service := &dashboard.Service{Discovery: &discovery.Discoverer{File: cfg.EndpointsFile, Image: cfg.DockerImage, Timeout: cfg.RequestTimeout, Docker: docker}, Upstream: up}
	var history *sqlitestore.Store
	if cfg.PersistenceEnabled {
		history, err = sqlitestore.Open(cfg.DatabasePath)
		if err != nil {
			return err
		}
		defer func() {
			if err := history.Close(); err != nil {
				slog.Warn("Failed to close persistence database", "error", err)
			}
		}()
		service.History = history
	}
	server := &http.Server{Addr: cfg.ListenAddr, Handler: httpserver.New(service, cfg.BasePath), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	var background sync.WaitGroup
	defer func() {
		stop()
		background.Wait()
	}()
	if service.History != nil {
		background.Go(func() {
			ticker := time.NewTicker(cfg.SyncInterval)
			defer ticker.Stop()
			for {
				failures, _ := service.Sync(ctx, "")
				for _, failure := range failures {
					slog.Warn("Persistence sync failed", "target", failure.Target, "operation", failure.Operation, "error", failure.Message)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		})
	}
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
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		cfg, err := config.Load()
		if err == nil {
			err = healthcheck(cfg)
		}
		if err != nil {
			slog.Error("Health check failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("Dashboard stopped", "error", err)
		os.Exit(1)
	}
}
