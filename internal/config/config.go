package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr     string
	EndpointsFile  string
	DockerImage    string
	RequestTimeout time.Duration
	MaxConcurrency int
	LogLevel       slog.Level
}

func Load() (Config, error) { return Parse(os.LookupEnv) }

func Parse(lookup func(string) (string, bool)) (Config, error) {
	get := func(key, fallback string) string {
		if value, ok := lookup("COPILOT_API_DASHBOARD_" + key); ok {
			return value
		}
		return fallback
	}
	c := Config{ListenAddr: get("LISTEN_ADDR", ":9000"), EndpointsFile: get("ENDPOINTS_FILE", "/config/endpoints.yaml"), DockerImage: get("DOCKER_IMAGE", "ghcr.io/caozhiyuan/copilot-api:latest")}
	_, port, err := net.SplitHostPort(c.ListenAddr)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 1 || n > 65535 {
		return c, fmt.Errorf("invalid LISTEN_ADDR")
	}
	if strings.TrimSpace(c.EndpointsFile) == "" || strings.TrimSpace(c.DockerImage) == "" {
		return c, fmt.Errorf("ENDPOINTS_FILE and DOCKER_IMAGE must not be empty")
	}
	c.RequestTimeout, err = time.ParseDuration(get("REQUEST_TIMEOUT", "5s"))
	if err != nil || c.RequestTimeout <= 0 {
		return c, fmt.Errorf("REQUEST_TIMEOUT must be a positive duration")
	}
	c.MaxConcurrency, err = strconv.Atoi(get("MAX_CONCURRENCY", "32"))
	if err != nil || c.MaxConcurrency <= 0 {
		return c, fmt.Errorf("MAX_CONCURRENCY must be a positive integer")
	}
	if err := c.LogLevel.UnmarshalText([]byte(get("LOG_LEVEL", "info"))); err != nil {
		return c, fmt.Errorf("invalid LOG_LEVEL")
	}
	return c, nil
}
