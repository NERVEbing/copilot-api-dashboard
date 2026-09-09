package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NERVEbing/copilot-api-dashboard/internal/discovery"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestContractFixtures(t *testing.T) {
	data := map[string][]byte{"/usage": fixture(t, "usage"), "/token-usage": fixture(t, "summary"), "/token-usage/daily": fixture(t, "daily"), "/token-usage/events": fixture(t, "events")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("x-api-key") != "test-key" {
			t.Error("incorrect upstream request")
		}
		if r.URL.Path != "/usage" && r.URL.Query().Get("period") != "last_30_days" {
			t.Error("missing period")
		}
		if r.URL.Path == "/token-usage/events" && (r.URL.Query().Get("page") != "2" || r.URL.Query().Get("page_size") != "1") {
			t.Error("wrong pagination")
		}
		_, _ = w.Write(data[r.URL.Path])
	}))
	defer srv.Close()
	c := New(time.Second, 2)
	defer c.Close()
	e := discovery.Endpoint{URL: srv.URL, APIKey: "test-key"}
	ctx := context.Background()
	u, err := c.Usage(ctx, e)
	if err != nil || u.Login != "Account-A" || !*u.Quotas.Chat.Unlimited || *u.Quotas.PremiumInteractions.Remaining != 270 {
		t.Fatalf("usage: %+v %v", u, err)
	}
	s, err := c.Summary(ctx, e, "last_30_days")
	if err != nil || s.Totals.Tokens != 180 || s.Totals.Requests != 2 || s.Totals.NanoAIU != nil || s.Models[0].Model != "model-a" || s.Totals.Costs[0].Nanos != 123 {
		t.Fatalf("summary: %+v %v", s, err)
	}
	d, err := c.Daily(ctx, e, "last_30_days")
	if err != nil || len(d.Days) != 1 || d.Days[0].Totals.CacheRead != 20 {
		t.Fatalf("daily: %+v %v", d, err)
	}
	p, err := c.Events(ctx, e, "last_30_days", 2, 1)
	if err != nil || p.Page != 2 || p.Items[0].Cost.Nanos != 123 || p.Items[0].UserID != "Account-A" {
		t.Fatalf("events: %+v %v", p, err)
	}
}

func TestRequestFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"non-2xx", "private-response", "HTTP 401", 401},
		{"redirect", "private-response", "HTTP 302", 302},
		{"malformed", "{private-response", "invalid upstream JSON", 200},
		{"trailing JSON", "{} {}", "invalid upstream JSON", 200},
		{"null", "null", "no valid login", 200},
		{"empty login", "{\"login\":\" \"}", "no valid login", 200},
		{"size", strings.Repeat(" ", MaxResponseBytes+1), "size limit", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Private", "test-key")
				w.Header().Set("Location", "http://elsewhere.invalid/?test-key")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := New(time.Second, 1)
			defer c.Close()
			_, err := c.Usage(context.Background(), discovery.Endpoint{URL: srv.URL, APIKey: "test-key"})
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "private-response") || strings.Contains(err.Error(), "test-key") {
				t.Fatalf("unsafe or wrong error: %v", err)
			}
		})
	}
}

func TestRedirectDoesNotReachDestination(t *testing.T) {
	var reached atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	c := New(time.Second, 1)
	defer c.Close()
	_, err := c.Usage(context.Background(), discovery.Endpoint{URL: source.URL, APIKey: "test-key"})
	if err == nil || reached.Load() != 0 {
		t.Fatal("redirect followed")
	}
}

func TestTimeoutAndGlobalConcurrency(t *testing.T) {
	var active, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case <-r.Context().Done():
			active.Add(-1)
		case <-time.After(5 * time.Millisecond):
			active.Add(-1)
			_, _ = w.Write([]byte(`{"login":"a"}`))
		}
	}))
	defer srv.Close()
	c := New(time.Second, 2)
	defer c.Close()
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if _, err := c.Usage(context.Background(), discovery.Endpoint{URL: srv.URL}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if peak.Load() > 2 {
		t.Fatalf("global concurrency %d", peak.Load())
	}
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer blocked.Close()
	short := New(20*time.Millisecond, 1)
	defer short.Close()
	_, err := short.Usage(context.Background(), discovery.Endpoint{URL: blocked.URL})
	if err == nil || err.Error() != "request timed out" {
		t.Fatalf("timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Usage(ctx, discovery.Endpoint{URL: srv.URL})
	if err == nil {
		t.Fatal("canceled request succeeded")
	}
}

func TestInvalidDatasetAndNullFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer srv.Close()
	c := New(time.Second, 1)
	defer c.Close()
	e := discovery.Endpoint{URL: srv.URL}
	if _, err := c.Summary(context.Background(), e, "today"); err == nil {
		t.Fatal("missing summary accepted")
	}
	if _, err := c.Daily(context.Background(), e, "today"); err == nil {
		t.Fatal("missing daily accepted")
	}
	if _, err := c.Events(context.Background(), e, "today", 1, 20); err == nil {
		t.Fatal("missing events accepted")
	}
	var u Usage
	if err := json.Unmarshal([]byte(`{"login":"a","quota_snapshots":{"chat":{"unlimited":false,"remaining":0}}}`), &u); err != nil {
		t.Fatal(err)
	}
	if u.Quotas.Chat.Remaining == nil || *u.Quotas.Chat.Remaining != 0 || u.Quotas.Chat.Entitlement != nil || u.Quotas.Completions != nil {
		t.Fatal("missing and zero quotas conflated")
	}
}
