package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NERVEbing/copilot-api-dashboard/internal/discovery"
	"github.com/NERVEbing/copilot-api-dashboard/internal/upstream"
)

type staticDiscovery []discovery.Endpoint

func (d staticDiscovery) Discover(context.Context) ([]discovery.Endpoint, []discovery.Failure) {
	return append([]discovery.Endpoint(nil), d...), nil
}

type target struct {
	endpoint discovery.Endpoint
	mu       sync.Mutex
	calls    map[string]int
}

func testTarget(t *testing.T, login string, tokens int64, fail string) *target {
	t.Helper()
	v := &target{calls: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		v.calls[r.URL.Path]++
		v.mu.Unlock()
		if r.URL.Path == fail {
			http.Error(w, "private upstream body", 503)
			return
		}
		period := r.URL.Query().Get("period")
		totals := upstream.Totals{Tokens: tokens, Input: tokens - 1, Output: 1, Requests: 1, Costs: []upstream.Cost{{Currency: "USD", Nanos: tokens, Amount: float64(tokens) / 1e9}}}
		summary := upstream.Summary{Period: period, Totals: &totals, Models: []upstream.Model{{Model: "model-a", Totals: totals}}}
		switch r.URL.Path {
		case "/usage":
			_ = json.NewEncoder(w).Encode(upstream.Usage{Login: login})
		case "/token-usage":
			_ = json.NewEncoder(w).Encode(summary)
		case "/token-usage/daily":
			_ = json.NewEncoder(w).Encode(upstream.Daily{Summary: summary, Days: []upstream.Day{{Date: "2026-09-08", Totals: &totals, Models: summary.Models}}})
		case "/token-usage/events":
			if r.URL.Query().Get("page") != "2" || r.URL.Query().Get("page_size") != "1" {
				t.Error("pagination changed")
			}
			_ = json.NewEncoder(w).Encode(upstream.Events{Items: []upstream.Event{{ID: 9, Tokens: tokens}}, Period: period, Page: 2, PageSize: 1, Total: 2, TotalPages: 2})
		}
	}))
	t.Cleanup(srv.Close)
	v.endpoint = discovery.Endpoint{Name: login, URL: srv.URL, Source: "yaml"}
	return v
}
func (t *target) count(path string) int { t.mu.Lock(); defer t.mu.Unlock(); return t.calls[path] }
func serviceFor(t *testing.T, targets ...*target) *Service {
	t.Helper()
	d := staticDiscovery{}
	for _, v := range targets {
		d = append(d, v.endpoint)
	}
	c := upstream.New(time.Second, 4)
	t.Cleanup(c.Close)
	return &Service{Discovery: d, Upstream: c}
}

func TestAccountAggregationAndSelection(t *testing.T) {
	a, b := testTarget(t, "Alice", 100, ""), testTarget(t, "Bob", 200, "")
	s := serviceFor(t, a, b)
	all, found := s.Dashboard(context.Background(), "today", "")
	if !found || len(all.Errors) != 0 || all.Data.Totals.Tokens != 300 || all.Data.Totals.Requests != 2 || all.Data.Models[0].Tokens != 300 || all.Data.Days[0].Totals.Tokens != 300 || all.Data.Days[0].Models[0].Tokens != 300 || all.Data.Totals.Costs[0].Nanos != 300 {
		t.Fatalf("wrong aggregate: %+v", all)
	}
	if all.Data.Accounts[0].Totals.Tokens != 100 || all.Data.Accounts[1].Totals.Tokens != 200 {
		t.Fatal("account data mutated by aggregation")
	}
	one, found := s.Dashboard(context.Background(), "today", "ALICE")
	if !found || *one.Data.SelectedAccount != "Alice" || one.Data.Totals.Tokens != 100 || len(one.Data.Accounts) != 2 || one.Data.Accounts[1].Totals != nil {
		t.Fatalf("selection: %+v", one)
	}
	if b.count("/usage") != 2 || b.count("/token-usage") != 1 || b.count("/token-usage/daily") != 1 {
		t.Fatal("selected request queried other token data")
	}
	events, status := s.Events(context.Background(), "alice", "today", 2, 1)
	if status != 200 || events.Data.Page != 2 || events.Data.Items[0].Tokens != 100 || b.count("/token-usage/events") != 0 {
		t.Fatal("wrong event endpoint")
	}
}

func TestDuplicateAccountWinner(t *testing.T) {
	a, b := testTarget(t, "Alice", 100, ""), testTarget(t, "ALICE", 200, "")
	a.endpoint.Source = "docker"
	s := serviceFor(t, a, b)
	out, _ := s.Dashboard(context.Background(), "today", "")
	if len(out.Data.Accounts) != 1 || out.Data.Totals.Tokens != 200 || len(out.Errors) != 0 || a.count("/token-usage") != 0 {
		t.Fatal("YAML winner not selected")
	}
	a.endpoint.Source = "yaml"
	s = serviceFor(t, a, b)
	out, _ = s.Dashboard(context.Background(), "today", "")
	want := int64(100)
	if b.endpoint.URL < a.endpoint.URL {
		want = 200
	}
	if out.Data.Totals.Tokens != want {
		t.Fatal("URL ordering ignored")
	}
}

func TestFailureIsolationAndUnavailable(t *testing.T) {
	for _, operation := range []string{"/usage", "/token-usage", "/token-usage/daily"} {
		t.Run(operation, func(t *testing.T) {
			a, b := testTarget(t, "Alice", 100, ""), testTarget(t, "Bob", 200, operation)
			s := serviceFor(t, a, b)
			out, _ := s.Dashboard(context.Background(), "today", "")
			if len(out.Errors) != 1 {
				t.Fatalf("errors: %+v", out.Errors)
			}
			wantTokens, wantDaily := int64(100), int64(100)
			if operation == "/token-usage" {
				wantDaily = 300
			}
			if operation == "/token-usage/daily" {
				wantTokens = 300
			}
			if out.Data.Totals.Tokens != wantTokens || out.Data.Days[0].Totals.Tokens != wantDaily {
				t.Fatal("healthy data lost")
			}
			if operation != "/usage" && len(out.Data.Accounts) != 2 {
				t.Fatal("account removed after dataset failure")
			}
		})
	}
	a := testTarget(t, "Alice", 100, "/token-usage")
	out, _ := serviceFor(t, a).Dashboard(context.Background(), "today", "")
	if out.Data.Totals != nil || out.Data.Models != nil || out.Data.Days == nil || out.Data.Accounts[0].Totals != nil {
		t.Fatal("unavailable summary converted to zero")
	}
	b := testTarget(t, "Bob", 100, "/token-usage/daily")
	out, _ = serviceFor(t, b).Dashboard(context.Background(), "today", "")
	if out.Data.Days != nil || out.Data.Totals == nil {
		t.Fatal("unavailable daily converted to empty array")
	}
	empty := serviceFor(t)
	out, found := empty.Dashboard(context.Background(), "today", "missing")
	if found || out.Data.Totals != nil || len(out.Errors) != 1 || out.Data.Accounts == nil {
		t.Fatal("unknown account response")
	}
	ev, status := empty.Events(context.Background(), "missing", "today", 2, 1)
	if status != 404 || ev.Data != nil {
		t.Fatal("unknown event account response")
	}
	c := testTarget(t, "Carol", 1, "/token-usage/events")
	ev, status = serviceFor(t, c).Events(context.Background(), "Carol", "today", 2, 1)
	if status != 502 || ev.Data != nil || len(ev.Errors) != 1 {
		t.Fatal("event failure response")
	}
}

func TestCurrencyAndNullableAIUAggregation(t *testing.T) {
	aiu := int64(7)
	a := upstream.Totals{Tokens: 3, CacheRead: 2, CacheCreation: 1, NanoAIU: &aiu, Costs: []upstream.Cost{{Currency: "USD", Nanos: 123, Amount: 99}, {Currency: "EUR", Nanos: 2}}}
	b := upstream.Totals{Tokens: 4, CacheRead: 3, CacheCreation: 2, Costs: []upstream.Cost{{Currency: "USD", Nanos: 456, Amount: 99}}}
	var got upstream.Totals
	addTotals(&got, a)
	addTotals(&got, b)
	if got.Tokens != 7 || got.CacheRead != 5 || got.CacheCreation != 3 || *got.NanoAIU != 7 || aiu != 7 || !reflect.DeepEqual(got.Costs, []upstream.Cost{{Currency: "EUR", Nanos: 2, Amount: 2e-9}, {Currency: "USD", Nanos: 579, Amount: 579e-9}}) {
		t.Fatalf("aggregation: %+v", got)
	}
	var absent upstream.Totals
	addTotals(&absent, b)
	if absent.NanoAIU != nil {
		t.Fatal("absent AIU became zero")
	}
	days := mergeDay(nil, upstream.Day{Date: "2026-09-08", Totals: &a, Models: []upstream.Model{{Model: "z", Totals: a}}})
	days = mergeDay(days, upstream.Day{Date: "2026-09-07", Totals: &b, Models: []upstream.Model{{Model: "a", Totals: b}}})
	if days[0].Date != "2026-09-07" || days[1].Totals.Tokens != 3 {
		t.Fatal("daily sorting or grouping")
	}
}

func TestSuccessfulEmptyAndFailedRefresh(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		switch r.URL.Path {
		case "/usage":
			_ = json.NewEncoder(w).Encode(upstream.Usage{Login: "empty"})
		case "/token-usage", "/token-usage/daily":
			_ = json.NewEncoder(w).Encode(upstream.Daily{Summary: upstream.Summary{Period: "today", Totals: &upstream.Totals{Costs: []upstream.Cost{}}, Models: []upstream.Model{}}, Days: []upstream.Day{}})
		}
	}))
	defer srv.Close()
	c := upstream.New(time.Second, 2)
	defer c.Close()
	s := &Service{Discovery: staticDiscovery{{Name: "empty", URL: srv.URL, Source: "yaml"}}, Upstream: c}
	out, _ := s.Dashboard(context.Background(), "today", "")
	if out.Data.Totals == nil || out.Data.Totals.Tokens != 0 || out.Data.Models == nil || len(out.Data.Models) != 0 || out.Data.Days == nil || len(out.Data.Days) != 0 {
		t.Fatalf("empty successful datasets: %+v", out)
	}
	fail.Store(true)
	out, _ = s.Dashboard(context.Background(), "today", "")
	if out.Data.Totals != nil || out.Data.Days != nil || len(out.Data.Accounts) != 0 || len(out.Errors) != 1 {
		t.Fatalf("failed refresh retained data: %+v", out)
	}
}
