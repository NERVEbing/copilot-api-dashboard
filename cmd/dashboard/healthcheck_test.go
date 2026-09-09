package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NERVEbing/copilot-api-dashboard/internal/config"
)

func TestHealthcheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilot-dashboard/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := config.Config{
		ListenAddr: strings.TrimPrefix(server.URL, "http://"),
		BasePath:   "/copilot-dashboard",
	}
	if err := healthcheck(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestHealthcheckRejectsUnhealthyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	cfg := config.Config{
		ListenAddr: strings.TrimPrefix(server.URL, "http://"),
		BasePath:   "/",
	}
	if err := healthcheck(cfg); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("unexpected error: %v", err)
	}
}
