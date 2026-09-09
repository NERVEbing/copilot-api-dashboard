package config

import (
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	c, err := Parse(lookup)
	if err != nil || c.ListenAddr != ":9000" || c.RequestTimeout != 5*time.Second || c.MaxConcurrency != 32 || c.EndpointsFile != "/config/endpoints.yaml" || c.DockerImage != "ghcr.io/caozhiyuan/copilot-api:latest" || c.LogLevel.String() != "INFO" {
		t.Fatalf("defaults: %+v, %v", c, err)
	}
	for _, tc := range []struct{ key, value string }{
		{"LISTEN_ADDR", "bad"}, {"LISTEN_ADDR", ":0"}, {"LISTEN_ADDR", ":65536"}, {"REQUEST_TIMEOUT", "0s"}, {"REQUEST_TIMEOUT", "-1s"}, {"REQUEST_TIMEOUT", "bad"}, {"MAX_CONCURRENCY", "0"}, {"MAX_CONCURRENCY", "-1"}, {"MAX_CONCURRENCY", "bad"}, {"LOG_LEVEL", "invalid"}, {"ENDPOINTS_FILE", ""}, {"DOCKER_IMAGE", ""},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			_, err := Parse(func(k string) (string, bool) { return tc.value, k == "COPILOT_API_DASHBOARD_"+tc.key })
			if err == nil {
				t.Fatal("expected configuration error")
			}
		})
	}
	c, err = Parse(func(k string) (string, bool) {
		v, ok := map[string]string{"LISTEN_ADDR": "127.0.0.1:9100", "REQUEST_TIMEOUT": "2s", "MAX_CONCURRENCY": "4", "LOG_LEVEL": "debug"}[strings.TrimPrefix(k, "COPILOT_API_DASHBOARD_")]
		return v, ok
	})
	if err != nil || c.MaxConcurrency != 4 || c.RequestTimeout != 2*time.Second || c.LogLevel.String() != "DEBUG" {
		t.Fatalf("overrides: %+v %v", c, err)
	}
}
