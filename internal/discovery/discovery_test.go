package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

func yamlFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "endpoints.yaml")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNormalizeURL(t *testing.T) {
	for raw, want := range map[string]string{"http://EXAMPLE:80/": "http://example", "https://EXAMPLE:443": "https://example", "http://[::1]:4141/": "http://[::1]:4141", "http://example:04141": "http://example:4141", "http://EXAMPLE/copilot/account/": "http://example/copilot/account"} {
		got, err := NormalizeURL(raw)
		if err != nil || got != want {
			t.Fatalf("%s: %s %v", raw, got, err)
		}
	}
	for _, raw := range []string{"", "ftp://example", "http://user:password@example", "http://example/a/../b", "http://example/a//b", "http://example?key=secret", "http://example?", "http://example#", "http://example/#x", "http://example:99999", "http:///", "http://example/%2f"} {
		if _, err := NormalizeURL(raw); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestYAML(t *testing.T) {
	lookup := func(k string) (string, bool) { return "test-credential", k == "TEST_KEY" }
	p := yamlFile(t, "endpoints:\n  - name: a\n    url: http://EXAMPLE:80/\n    api_key_env: TEST_KEY\n")
	items, err := LoadYAML(p, lookup)
	if err != nil || len(items) != 1 || items[0].URL != "http://example" || items[0].APIKey != "test-credential" {
		t.Fatalf("load failed: %v", err)
	}
	if _, err := LoadYAML(filepath.Join(t.TempDir(), "missing"), lookup); err != nil {
		t.Fatal(err)
	}
	p = yamlFile(t, "endpoints:\n  - name: a\n    url: http://example\n    api_key: direct-credential\n")
	items, err = LoadYAML(p, lookup)
	if err != nil || len(items) != 1 || items[0].APIKey != "direct-credential" {
		t.Fatalf("load direct credential failed: %v", err)
	}
	for _, body := range []string{"secret: test-credential", "endpoints:\n - name: a\n   url: http://a\n   api_key: test-credential\n   api_key_env: TEST_KEY", "endpoints:\n - name: a", "endpoints:\n - name: a\n   url: http://a\n   api_key: ''", "endpoints:\n - name: a\n   url: http://a\n   api_key_env: MISSING", "endpoints:\n - name: a\n   url: http://a\n   api_key_env: ''", "endpoints: []\n---\nendpoints: []", "endpoints: [", "endpoints: []\n" + strings.Repeat("#", 1<<20)} {
		_, err := LoadYAML(yamlFile(t, body), lookup)
		if err == nil {
			t.Errorf("accepted invalid YAML")
		} else if strings.Contains(err.Error(), "test-credential") {
			t.Fatal("credential exposed")
		}
	}
	items = Deduplicate([]Endpoint{{Name: "z", URL: "http://a", Source: "docker"}, {Name: "b", URL: "http://a", Source: "yaml", APIKey: "chosen"}, {Name: "a", URL: "http://b", Source: "yaml"}, {Name: "a", URL: "http://docker-a", Source: "docker"}})
	if len(items) != 2 || items[0].Name != "b" || items[0].APIKey != "chosen" {
		t.Fatal("wrong duplicate winner")
	}
}

type fakeDocker struct {
	listErr    error
	inspectErr map[string]error
	seen       []string
	block      bool
}

func (f *fakeDocker) ContainerList(ctx context.Context, o client.ContainerListOptions) (client.ContainerListResult, error) {
	if f.block {
		<-ctx.Done()
		return client.ContainerListResult{}, ctx.Err()
	}
	return client.ContainerListResult{Items: []container.Summary{
		{ID: "ok", Names: []string{"/ok"}, Image: "wanted", State: "running"}, {ID: "bad", Names: []string{"/bad"}, Image: "wanted", State: "running"}, {ID: "wrong-image", Image: "wanted:other", State: "running"}, {ID: "stopped", Image: "wanted", State: "exited"},
	}}, f.listErr
}
func (f *fakeDocker) ContainerInspect(ctx context.Context, id string, o client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.seen = append(f.seen, id)
	return client.ContainerInspectResult{Container: container.InspectResponse{Name: "/" + id, State: &container.State{Running: true}, Config: &container.Config{Env: []string{"UNRELATED=ignored", "COPILOT_API_KEY=test-key"}}}}, f.inspectErr[id]
}

func TestDiscoveryIsolation(t *testing.T) {
	for _, tc := range []struct {
		name, body                string
		listErr                   error
		wantEndpoints, wantErrors int
	}{
		{"yaml fails", "broken: secret", nil, 1, 2},
		{"docker fails", "endpoints:\n - name: yaml\n   url: http://yaml", errors.New("remote secret"), 1, 1},
		{"inspect fails", "endpoints:\n - name: yaml\n   url: http://yaml", nil, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeDocker{listErr: tc.listErr, inspectErr: map[string]error{"bad": errors.New("remote secret")}}
			d := Discoverer{File: yamlFile(t, tc.body), Image: "wanted", Timeout: time.Second, Docker: f, SocketExists: func() (bool, error) { return true, nil }}
			items, errs := d.Discover(context.Background())
			if len(items) != tc.wantEndpoints || len(errs) != tc.wantErrors {
				t.Fatalf("endpoints=%d errors=%+v", len(items), errs)
			}
			for _, e := range errs {
				if strings.Contains(e.Message, "secret") {
					t.Fatal("remote error exposed")
				}
			}
			for _, id := range f.seen {
				if id != "ok" && id != "bad" {
					t.Fatal("inspected nonmatching container")
				}
			}
			for _, e := range items {
				if e.Source == "docker" && (e.URL != "http://ok:4141" || e.APIKey != "test-key") {
					t.Fatal("incorrect Docker endpoint")
				}
			}
		})
	}
}

func TestDiscoveryTimeoutAndMissingSocket(t *testing.T) {
	d := Discoverer{File: yamlFile(t, "endpoints:\n - name: yaml\n   url: http://yaml"), Image: "wanted", Timeout: 10 * time.Millisecond, Docker: &fakeDocker{block: true}, SocketExists: func() (bool, error) { return true, nil }}
	items, errs := d.Discover(context.Background())
	if len(items) != 1 || len(errs) != 1 || errs[0].Message != "request timed out" {
		t.Fatalf("timeout: %d %+v", len(items), errs)
	}
	d.SocketExists = func() (bool, error) { return false, nil }
	items, errs = d.Discover(context.Background())
	if len(items) != 1 || len(errs) != 0 {
		t.Fatalf("missing socket: %d %+v", len(items), errs)
	}
}
