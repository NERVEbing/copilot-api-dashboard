package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	t.Chdir(t.TempDir())
	lookup := func(string) (string, bool) { return "", false }
	c, err := Parse(lookup)
	if err != nil || c.ListenAddr != ":9000" || c.BasePath != "/" || c.RequestTimeout != 5*time.Second || c.MaxConcurrency != 32 || c.EndpointsFile != "/config/endpoints.yaml" || c.DockerImage != "ghcr.io/caozhiyuan/copilot-api:latest" || c.LogLevel.String() != "INFO" {
		t.Fatalf("defaults: %+v, %v", c, err)
	}
	for _, tc := range []struct{ key, value string }{
		{"LISTEN_ADDR", "bad"}, {"LISTEN_ADDR", ":0"}, {"LISTEN_ADDR", ":65536"}, {"BASE_PATH", ""}, {"BASE_PATH", "dashboard"}, {"BASE_PATH", "/dashboard//nested"}, {"BASE_PATH", "/dashboard/../admin"}, {"BASE_PATH", "/review/{bad"}, {"BASE_PATH", "/dashboard?debug=1"}, {"BASE_PATH", "/dashboard%2Fadmin"}, {"REQUEST_TIMEOUT", "0s"}, {"REQUEST_TIMEOUT", "-1s"}, {"REQUEST_TIMEOUT", "bad"}, {"MAX_CONCURRENCY", "0"}, {"MAX_CONCURRENCY", "-1"}, {"MAX_CONCURRENCY", "bad"}, {"LOG_LEVEL", "invalid"}, {"ENDPOINTS_FILE", ""}, {"DOCKER_IMAGE", ""},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			_, err := Parse(func(k string) (string, bool) { return tc.value, k == "COPILOT_API_DASHBOARD_"+tc.key })
			if err == nil {
				t.Fatal("expected configuration error")
			}
		})
	}
	c, err = Parse(func(k string) (string, bool) {
		v, ok := map[string]string{"LISTEN_ADDR": "127.0.0.1:9100", "BASE_PATH": "/copilot-dashboard/", "REQUEST_TIMEOUT": "2s", "MAX_CONCURRENCY": "4", "LOG_LEVEL": "debug"}[strings.TrimPrefix(k, "COPILOT_API_DASHBOARD_")]
		return v, ok
	})
	if err != nil || c.BasePath != "/copilot-dashboard" || c.MaxConcurrency != 4 || c.RequestTimeout != 2*time.Second || c.LogLevel.String() != "DEBUG" {
		t.Fatalf("overrides: %+v %v", c, err)
	}
}

func TestParsePrefersLocalEndpointsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "config"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config", "endpoints.yaml"), []byte("endpoints: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	c, err := Parse(func(string) (string, bool) { return "", false })
	if err != nil || c.EndpointsFile != "config/endpoints.yaml" {
		t.Fatalf("local endpoints: %+v %v", c, err)
	}
	c, err = Parse(func(key string) (string, bool) {
		return "/custom/endpoints.yaml", key == "COPILOT_API_DASHBOARD_ENDPOINTS_FILE"
	})
	if err != nil || c.EndpointsFile != "/custom/endpoints.yaml" {
		t.Fatalf("explicit endpoints: %+v %v", c, err)
	}
}
