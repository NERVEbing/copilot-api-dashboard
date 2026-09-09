package main

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/NERVEbing/copilot-api-dashboard/internal/config"
)

func healthcheck(cfg config.Config) error {
	host, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	path := "/healthz"
	if cfg.BasePath != "/" {
		path = cfg.BasePath + path
	}
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://" + net.JoinHostPort(host, port) + path)
	if err != nil {
		return fmt.Errorf("health endpoint request failed: %w", err)
	}
	defer func() {
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint returned HTTP %d", response.StatusCode)
	}
	return nil
}
