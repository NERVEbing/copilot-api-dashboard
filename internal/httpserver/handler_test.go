package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NERVEbing/copilot-api-dashboard/internal/dashboard"
	"github.com/NERVEbing/copilot-api-dashboard/internal/discovery"
	"github.com/NERVEbing/copilot-api-dashboard/internal/upstream"
)

type noDiscovery struct{}

func (noDiscovery) Discover(context.Context) ([]discovery.Endpoint, []discovery.Failure) {
	return nil, nil
}

func TestRoutesAndValidation(t *testing.T) {
	c := upstream.New(time.Second, 2)
	defer c.Close()
	h := New(&dashboard.Service{Discovery: noDiscovery{}, Upstream: c}, "/")
	for _, tc := range []struct {
		path    string
		status  int
		content string
	}{
		{"/", 200, "text/html"}, {"/app.js", 200, "text/javascript"}, {"/styles.css", 200, "text/css"}, {"/healthz", 200, "application/json"},
		{"/unknown", 404, "text/plain"}, {"/api/v1/unknown", 404, "text/plain"}, {"/index.html", 404, "text/plain"}, {"/nested/app.js", 404, "text/plain"}, {"/app.js/", 404, "text/plain"},
		{"/api/v1/dashboard", 200, "application/json"}, {"/api/v1/dashboard?period=bad", 400, "application/json"}, {"/api/v1/dashboard?period=", 400, "application/json"}, {"/api/v1/dashboard?account=", 400, "application/json"},
		{"/api/v1/dashboard?account=missing", 404, "application/json"}, {"/api/v1/dashboard?period=today&period=lifetime", 400, "application/json"}, {"/api/v1/dashboard?period=%zz", 400, "application/json"}, {"/api/v1/dashboard?page=1", 400, "application/json"},
		{"/api/v1/events", 400, "application/json"}, {"/api/v1/events?account=missing", 404, "application/json"}, {"/api/v1/events?account=a&page=0", 400, "application/json"}, {"/api/v1/events?account=a&page_size=101", 400, "application/json"}, {"/api/v1/events?account=a&page=2x", 400, "application/json"}, {"/api/v1/events?account=a&page_size=", 400, "application/json"},
		{"/api/v1/sync", 405, "text/plain"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
			if w.Code != tc.status || !strings.HasPrefix(w.Header().Get("Content-Type"), tc.content) {
				t.Fatalf("response %d %s", w.Code, w.Body.String())
			}
		})
	}
	for _, period := range []string{"today", "this_week", "last_7_days", "this_month", "last_30_days", "lifetime"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/dashboard?period="+period, nil))
		if w.Code != 200 {
			t.Fatal("period rejected", period)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/dashboard", nil))
	if w.Code != 405 || w.Header().Get("Allow") != "GET" {
		t.Fatal("method validation")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/sync", nil))
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("sync response: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/sync?period=today", nil))
	if w.Code != 400 {
		t.Fatalf("sync query validation: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	crossSite := httptest.NewRequest("POST", "/api/v1/sync", nil)
	crossSite.Header.Set("Sec-Fetch-Site", "cross-site")
	h.ServeHTTP(w, crossSite)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-site sync: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/dashboard", nil))
	var got struct {
		Data struct {
			Period   string `json:"period"`
			Accounts []any  `json:"accounts"`
			Totals   any    `json:"totals"`
			Days     any    `json:"days"`
		}
		Errors []any
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Data.Period != "last_30_days" || got.Data.Accounts == nil || got.Errors == nil || got.Data.Totals != nil || got.Data.Days != nil || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("empty response: %s", w.Body.String())
	}
}

func TestHTTPVerticalSlice(t *testing.T) {
	fixtures := map[string][]byte{}
	for path, name := range map[string]string{"/usage": "usage", "/token-usage": "summary", "/token-usage/daily": "daily", "/token-usage/events": "events"} {
		b, err := os.ReadFile("../upstream/testdata/" + name + ".json")
		if err != nil {
			t.Fatal(err)
		}
		fixtures[path] = b
	}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "private-test-key" {
			t.Error("missing key")
		}
		_, _ = w.Write(fixtures[r.URL.Path])
	}))
	defer source.Close()
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte("private-test-key"))
	}))
	defer failed.Close()
	file := filepath.Join(t.TempDir(), "endpoints.yaml")
	body := "endpoints:\n - name: healthy\n   url: " + source.URL + "\n   api_key_env: TEST_KEY\n - name: failed\n   url: " + failed.URL + "\n"
	if err := os.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	d := &discovery.Discoverer{File: file, Timeout: time.Second, SocketExists: func() (bool, error) { return false, nil }, LookupEnv: func(key string) (string, bool) { return "private-test-key", key == "TEST_KEY" }}
	c := upstream.New(time.Second, 2)
	defer c.Close()
	srv := httptest.NewServer(New(&dashboard.Service{Discovery: d, Upstream: c}, "/"))
	defer srv.Close()
	res, err := http.Get(srv.URL + "/api/v1/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			t.Errorf("close dashboard response body: %v", err)
		}
	}()
	var out dashboard.Response
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || out.Data.Totals.Tokens != 180 || len(out.Errors) != 1 || out.Errors[0].Target != "failed" || out.Errors[0].Message != "upstream returned HTTP 503" {
		t.Fatalf("dashboard: %+v", out)
	}
	encoded, _ := json.Marshal(out)
	if strings.Contains(string(encoded), "private-test-key") {
		t.Fatal("credential leaked")
	}
	res2, err := http.Get(srv.URL + "/api/v1/events?account=account-a&page=2&page_size=1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := res2.Body.Close(); err != nil {
			t.Errorf("close events response body: %v", err)
		}
	}()
	var events dashboard.EventsResponse
	if err := json.NewDecoder(res2.Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	if res2.StatusCode != 200 || events.Data.Page != 2 || events.Data.PageSize != 1 || events.Data.Items[0].Cost.Nanos != 123 || len(events.Errors) != 1 {
		t.Fatalf("events: %+v", events)
	}
}

func TestBasePathRoutes(t *testing.T) {
	c := upstream.New(time.Second, 2)
	defer c.Close()
	h := New(&dashboard.Service{Discovery: noDiscovery{}, Upstream: c}, "/copilot-dashboard")

	for _, tc := range []struct {
		path    string
		status  int
		content string
	}{
		{"/", 404, "text/plain"},
		{"/app.js", 404, "text/plain"},
		{"/api/v1/dashboard", 404, "text/plain"},
		{"/copilot-dashboard/", 200, "text/html"},
		{"/copilot-dashboard/app.js", 200, "text/javascript"},
		{"/copilot-dashboard/styles.css", 200, "text/css"},
		{"/copilot-dashboard/healthz", 200, "application/json"},
		{"/copilot-dashboard/api/v1/dashboard", 200, "application/json"},
		{"/copilot-dashboard/api/v1/sync", 405, "text/plain"},
		{"/copilot-dashboard/unknown", 404, "text/plain"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != tc.status || !strings.HasPrefix(w.Header().Get("Content-Type"), tc.content) {
				t.Fatalf("response %d %s", w.Code, w.Body.String())
			}
			if tc.path == "/copilot-dashboard/" && (!strings.Contains(w.Body.String(), `href="./styles.css"`) || !strings.Contains(w.Body.String(), `src="./app.js"`)) {
				t.Fatalf("HTML assets are not base-path relative: %s", w.Body.String())
			}
		})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/copilot-dashboard?period=today", nil))
	if w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != "/copilot-dashboard/?period=today" {
		t.Fatalf("base path redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/copilot-dashboard/api/v1/sync", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("base path sync: %d %s", w.Code, w.Body.String())
	}
}
