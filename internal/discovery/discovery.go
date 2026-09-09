package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/client"
	"go.yaml.in/yaml/v3"
)

const SocketPath = "/var/run/docker.sock"

type Endpoint struct {
	Name   string
	URL    string
	Source string
	APIKey string `json:"-"`
}

type Failure struct {
	Target    string `json:"target"`
	Operation string `json:"operation"`
	Message   string `json:"message"`
}

// SafeMessage classifies transport failures without exposing remote error text.
func SafeMessage(err error) string {
	if errors.Is(err, context.Canceled) {
		return "request canceled"
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return "request timed out"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "hostname resolution failed"
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return "network connection failed"
	}
	return "request failed"
}

func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return "", errors.New("invalid endpoint URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || (u.EscapedPath() != "" && u.EscapedPath() != "/") || u.Opaque != "" {
		return "", errors.New("invalid endpoint URL")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("invalid endpoint port")
		}
		port = strconv.Itoa(n)
	}
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	u.Path, u.RawPath = "", ""
	return u.String(), nil
}

func Less(a, b Endpoint) bool {
	if a.Source != b.Source {
		return a.Source == "yaml"
	}
	if a.URL != b.URL {
		return a.URL < b.URL
	}
	return a.Name < b.Name
}

func Deduplicate(items []Endpoint) []Endpoint {
	sort.SliceStable(items, func(i, j int) bool { return Less(items[i], items[j]) })
	out := make([]Endpoint, 0, len(items))
	seen := map[string]bool{}
	for _, e := range items {
		if !seen[e.URL] {
			seen[e.URL] = true
			out = append(out, e)
		}
	}
	return out
}

func LoadYAML(path string, lookup func(string) (string, bool)) ([]Endpoint, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("cannot read endpoint file")
	}
	defer func() { _ = f.Close() }()
	var doc struct {
		Endpoints []struct {
			Name   string  `yaml:"name"`
			URL    string  `yaml:"url"`
			KeyEnv *string `yaml:"api_key_env"`
		} `yaml:"endpoints"`
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, errors.New("cannot read endpoint file")
	}
	if len(data) > 1<<20 {
		return nil, errors.New("endpoint YAML exceeds size limit")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return nil, errors.New("invalid endpoint YAML")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("endpoint YAML must contain one document")
	}
	items := make([]Endpoint, 0, len(doc.Endpoints))
	for i, entry := range doc.Endpoints {
		base, err := NormalizeURL(entry.URL)
		if strings.TrimSpace(entry.Name) == "" || err != nil {
			return nil, fmt.Errorf("invalid endpoint entry %d: name and valid root URL required", i+1)
		}
		key := ""
		if entry.KeyEnv != nil {
			var ok bool
			key, ok = lookup(*entry.KeyEnv)
			if !ok || strings.TrimSpace(key) == "" || *entry.KeyEnv == "" {
				return nil, fmt.Errorf("invalid endpoint entry %d: credential environment variable is empty or missing", i+1)
			}
		}
		items = append(items, Endpoint{Name: entry.Name, URL: base, Source: "yaml", APIKey: key})
	}
	return items, nil
}

type DockerClient interface {
	ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
}

type Discoverer struct {
	File         string
	Image        string
	Timeout      time.Duration
	Docker       DockerClient
	LookupEnv    func(string) (string, bool)
	SocketExists func() (bool, error)
}

func (d *Discoverer) Discover(ctx context.Context) ([]Endpoint, []Failure) {
	lookup := d.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	items, err := LoadYAML(d.File, lookup)
	failures := []Failure{}
	if err != nil {
		failures = append(failures, Failure{"yaml", "discovery", err.Error()})
	}
	exists := d.SocketExists
	if exists == nil {
		exists = func() (bool, error) {
			_, err := os.Stat(SocketPath)
			if errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			return err == nil, err
		}
	}
	present, err := exists()
	if err != nil {
		failures = append(failures, Failure{"docker", "discovery", "cannot access Docker socket"})
	}
	if !present || err != nil {
		return Deduplicate(items), failures
	}
	if d.Docker == nil {
		return Deduplicate(items), append(failures, Failure{"docker", "discovery", "Docker client unavailable"})
	}
	callCtx, cancel := context.WithTimeout(ctx, d.Timeout)
	containers, err := d.Docker.ContainerList(callCtx, client.ContainerListOptions{})
	cancel()
	if err != nil {
		return Deduplicate(items), append(failures, Failure{"docker", "list", SafeMessage(err)})
	}
	for _, c := range containers.Items {
		if c.Image != d.Image || c.State != "running" {
			continue
		}
		name := c.ID
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		callCtx, cancel := context.WithTimeout(ctx, d.Timeout)
		result, err := d.Docker.ContainerInspect(callCtx, c.ID, client.ContainerInspectOptions{})
		cancel()
		if err != nil {
			failures = append(failures, Failure{name, "inspect", SafeMessage(err)})
			continue
		}
		v := result.Container
		if v.State == nil || v.Config == nil || v.Name == "" {
			failures = append(failures, Failure{name, "inspect", "invalid container response"})
			continue
		}
		if !v.State.Running {
			continue
		}
		name = strings.TrimPrefix(v.Name, "/")
		base, err := NormalizeURL("http://" + name + ":4141")
		if err != nil {
			failures = append(failures, Failure{name, "inspect", "invalid container name"})
			continue
		}
		key := ""
		for _, env := range v.Config.Env {
			if value, ok := strings.CutPrefix(env, "COPILOT_API_KEY="); ok {
				key = value
			}
		}
		items = append(items, Endpoint{Name: name, URL: base, Source: "docker", APIKey: key})
	}
	return Deduplicate(items), failures
}
