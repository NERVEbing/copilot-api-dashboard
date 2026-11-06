package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
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

type discoveryFunc func(context.Context) ([]discovery.Endpoint, []discovery.Failure)

func (f discoveryFunc) Discover(ctx context.Context) ([]discovery.Endpoint, []discovery.Failure) {
	return f(ctx)
}

type memoryHistory struct {
	days        []upstream.Day
	hasSnapshot bool
	saveCalls   int
	loadCalls   int
	period      string
	login       string
}

type synchronizedHistory struct {
	mu        sync.Mutex
	saveCalls map[string]int
	save      func(context.Context, string)
}

func (h *synchronizedHistory) SaveDaily(ctx context.Context, _, name, _, _ string, _ *upstream.Daily) error {
	h.mu.Lock()
	if h.saveCalls == nil {
		h.saveCalls = map[string]int{}
	}
	h.saveCalls[name]++
	h.mu.Unlock()
	if h.save != nil {
		h.save(ctx, name)
	}
	return nil
}

func (h *synchronizedHistory) LoadDaily(context.Context, string, string, string, string, time.Time) ([]upstream.Day, bool, error) {
	return nil, false, nil
}

func (h *synchronizedHistory) count(name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.saveCalls[name]
}

func (h *memoryHistory) SaveDaily(_ context.Context, _, _, _, _ string, daily *upstream.Daily) error {
	h.saveCalls++
	h.period = daily.Period
	return nil
}

func (h *memoryHistory) LoadDaily(_ context.Context, _, _, login, period string, _ time.Time) ([]upstream.Day, bool, error) {
	h.loadCalls++
	h.period = period
	h.login = login
	return h.days, h.hasSnapshot, nil
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
			http.Error(w, "private upstream body", http.StatusServiceUnavailable)
			return
		}
		period := r.URL.Query().Get("period")
		totals := upstream.Totals{Tokens: tokens, Input: tokens - 1, Output: 1, Requests: 1, Costs: []upstream.Cost{{Currency: "USD", Nanos: tokens, Amount: float64(tokens) / 1e9}}}
		summary := upstream.Summary{Period: period, Totals: &totals, Models: []upstream.Model{{Model: "model-a", Totals: totals}}}
		switch r.URL.Path {
		case "/usage":
			_ = json.NewEncoder(w).Encode(upstream.Usage{Login: login})
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
	if b.count("/usage") != 2 || b.count("/token-usage/daily") != len(upstream.Periods) {
		t.Fatal("selection queried upstream instead of the snapshot")
	}
	events, status := s.Events(context.Background(), "alice", "today", 2, 1)
	if status != 200 || events.Data.Page != 2 || events.Data.Items[0].Tokens != 100 || b.count("/token-usage/events") != 0 {
		t.Fatal("wrong event endpoint")
	}
}

func TestAccountAliasesPreserveIdentity(t *testing.T) {
	for _, mode := range []string{"live", "persistent"} {
		t.Run(mode, func(t *testing.T) {
			a := testTarget(t, "SESA836035_seemu", 100, "")
			b := testTarget(t, "SESA676739_seemu", 200, "")
			c := testTarget(t, "Unmapped", 300, "")
			s := serviceFor(t, a, b, c)
			s.AccountAliases = map[string]string{"sesa836035_seemu": "张三", "sesa676739_seemu": "张三"}
			history := &memoryHistory{hasSnapshot: true, days: []upstream.Day{{Date: "2026-09-08", Totals: &upstream.Totals{Tokens: 100}}}}
			if mode == "persistent" {
				s.History = history
			}
			all, found := s.Dashboard(context.Background(), "lifetime", "")
			if !found || len(all.Errors) != 0 || len(all.Data.Accounts) != 3 {
				t.Fatalf("accounts: %+v", all)
			}
			for _, account := range all.Data.Accounts {
				want := "张三"
				if account.Login == "Unmapped" {
					want = ""
				}
				if account.DisplayName != want {
					t.Fatalf("account alias: %+v", account)
				}
			}
			one, found := s.Dashboard(context.Background(), "lifetime", "sesa836035_seemu")
			if !found || len(one.Errors) != 0 || one.Data.SelectedAccount == nil || *one.Data.SelectedAccount != "SESA836035_seemu" || one.Data.Totals.Tokens != 100 {
				t.Fatalf("selected account: %+v", one)
			}
			if mode == "persistent" && history.login != "SESA836035_seemu" {
				t.Fatalf("history login: %s", history.login)
			}
			encoded, err := json.Marshal(all.Data.Accounts)
			if err != nil {
				t.Fatal(err)
			}
			var accounts []map[string]any
			if err := json.Unmarshal(encoded, &accounts); err != nil {
				t.Fatal(err)
			}
			for _, account := range accounts {
				if account["login"] == "Unmapped" {
					if _, exists := account["display_name"]; exists {
						t.Fatal("unmapped account has a display_name field")
					}
				}
			}
		})
	}
}

func TestDuplicateAccountWinner(t *testing.T) {
	a, b := testTarget(t, "Alice", 100, ""), testTarget(t, "ALICE", 200, "")
	a.endpoint.Source = "docker"
	s := serviceFor(t, a, b)
	out, _ := s.Dashboard(context.Background(), "today", "")
	if len(out.Data.Accounts) != 1 || out.Data.Totals.Tokens != 200 || len(out.Errors) != 0 {
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
	for _, operation := range []string{"/usage", "/token-usage/daily"} {
		t.Run(operation, func(t *testing.T) {
			a, b := testTarget(t, "Alice", 100, ""), testTarget(t, "Bob", 200, operation)
			s := serviceFor(t, a, b)
			out, _ := s.Dashboard(context.Background(), "today", "")
			if len(out.Errors) != 1 {
				t.Fatalf("errors: %+v", out.Errors)
			}
			if out.Data.Totals.Tokens != 100 || out.Data.Days[0].Totals.Tokens != 100 {
				t.Fatal("healthy data lost")
			}
			if operation != "/usage" && len(out.Data.Accounts) != 2 {
				t.Fatal("account removed after dataset failure")
			}
		})
	}
	b := testTarget(t, "Bob", 100, "/token-usage/daily")
	out, _ := serviceFor(t, b).Dashboard(context.Background(), "today", "")
	if out.Data.Totals != nil || out.Data.Models != nil || out.Data.Days != nil || out.Data.Accounts[0].Totals != nil {
		t.Fatal("unavailable usage converted to zero")
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

func TestFillDaysUsesPeriodBoundariesAndPreservesRecordedValues(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.Local)
	recorded := Day{Date: "2026-09-09", Recorded: true, Totals: upstream.Totals{Tokens: 10}, Models: []upstream.Model{}}
	tests := []struct {
		name, period, first string
		days                []Day
		length              int
	}{
		{name: "last seven", period: "last_7_days", first: "2026-09-03", days: []Day{recorded}, length: 7},
		{name: "last thirty empty", period: "last_30_days", first: "2026-08-11", days: []Day{}, length: 30},
		{name: "this week", period: "this_week", first: "2026-09-07", days: []Day{recorded}, length: 3},
		{name: "this month", period: "this_month", first: "2026-09-01", days: []Day{recorded}, length: 9},
		{name: "lifetime minimum", period: "lifetime", first: "2026-09-03", days: []Day{recorded}, length: 7},
		{name: "lifetime history", period: "lifetime", first: "2026-08-01", days: []Day{{Date: "2026-08-01", Recorded: true}}, length: 40},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := fillDays(test.days, test.period, now)
			if len(got) != test.length || got[0].Date != test.first || got[len(got)-1].Date != "2026-09-09" {
				t.Fatalf("filled days: %+v", got)
			}
			if len(test.days) > 0 {
				recordedPreserved := false
				for _, day := range got {
					if day.Date == test.days[0].Date && day.Recorded == test.days[0].Recorded && day.Totals.Tokens == test.days[0].Totals.Tokens {
						recordedPreserved = true
					}
				}
				if !recordedPreserved {
					t.Fatal("recorded day replaced by placeholder")
				}
			}
			for _, day := range got {
				if !day.Recorded && (day.Totals.Tokens != 0 || day.Totals.Requests != 0 || day.Totals.Costs == nil || day.Models == nil) {
					t.Fatalf("invalid placeholder: %+v", day)
				}
			}
		})
	}
	if got := fillDays([]Day{}, "lifetime", now); len(got) != 0 {
		t.Fatalf("empty lifetime filled: %+v", got)
	}
	ancient := Day{Date: "0001-01-01", Recorded: true, Totals: upstream.Totals{Tokens: 1}, Models: []upstream.Model{}}
	if got := fillDays([]Day{ancient}, "lifetime", now); len(got) != 1 || got[0].Date != ancient.Date || !got[0].Recorded {
		t.Fatalf("unbounded lifetime was filled: %+v", got)
	}
	if got := fillDays([]Day{recorded}, "today", now); len(got) != 1 || !got[0].Recorded {
		t.Fatalf("today changed: %+v", got)
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
		case "/token-usage/daily":
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
	if failures, _ := s.Sync(context.Background(), ""); len(failures) != 1 {
		t.Fatalf("failed refresh: %+v", failures)
	}
	out, _ = s.Dashboard(context.Background(), "today", "")
	if out.Data.Totals != nil || out.Data.Days != nil || len(out.Data.Accounts) != 0 || len(out.Errors) != 1 {
		t.Fatalf("failed refresh retained data: %+v", out)
	}
}

func TestRequestsShareAccountSnapshotUntilSync(t *testing.T) {
	// Calls of /usage and /token-usage/daily after the first round, after events, and after a sync.
	// Without persistence the first round confirms the login before and after its reads; later rounds
	// start from the login confirmed last. With persistence the sync confirms the login after its read.
	want := map[string][3][2]int{
		"live":       {{2, 6}, {3, 6}, {4, 12}},
		"persistent": {{1, 0}, {2, 0}, {4, 1}},
	}
	for _, mode := range []string{"live", "persistent"} {
		t.Run(mode, func(t *testing.T) {
			a := testTarget(t, "Alice", 100, "")
			s := serviceFor(t, a)
			if mode == "persistent" {
				s.History = &memoryHistory{hasSnapshot: true, days: []upstream.Day{}}
			}
			var now atomic.Int64
			now.Store(time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC).UnixMilli())
			s.Now = func() time.Time { return time.UnixMilli(now.Load()) }
			calls := func() [2]int { return [2]int{a.count("/usage"), a.count("/token-usage/daily")} }
			first, _ := s.Dashboard(context.Background(), "today", "")
			one, found := s.Dashboard(context.Background(), "lifetime", "alice")
			if got := calls(); !found || got != want[mode][0] || first.Data.UpdatedAtMS == nil || *first.Data.UpdatedAtMS != now.Load() || one.Data.UpdatedAtMS == nil || *one.Data.UpdatedAtMS != now.Load() {
				t.Fatalf("dashboards did not share one round: calls=%v first=%+v one=%+v", got, first.Data, one.Data)
			}
			if mode == "live" && (first.Data.Totals.Tokens != 100 || one.Data.Totals.Tokens != 100) {
				t.Fatalf("usage not served from the round: first=%+v one=%+v", first.Data.Totals, one.Data.Totals)
			}
			if _, status := s.Events(context.Background(), "Alice", "today", 2, 1); status != 200 || calls() != want[mode][1] {
				t.Fatalf("events did not confirm the account without a new round: status=%d calls=%v", status, calls())
			}
			now.Add(time.Minute.Milliseconds())
			if failures, found := s.Sync(context.Background(), ""); !found || len(failures) != 0 {
				t.Fatalf("sync: %+v", failures)
			}
			out, _ := s.Dashboard(context.Background(), "today", "")
			if got := calls(); got != want[mode][2] || out.Data.UpdatedAtMS == nil || *out.Data.UpdatedAtMS != now.Load() {
				t.Fatalf("sync did not take a new round: calls=%v data=%+v", got, out.Data)
			}
		})
	}
}

func TestCanceledRequestDoesNotCancelOrReplaceSharedSnapshot(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var usageCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/usage":
			if usageCalls.Add(1) == 1 {
				close(started)
				<-release
			}
			_ = json.NewEncoder(w).Encode(upstream.Usage{Login: "Alice"})
		case "/token-usage/daily":
			_ = json.NewEncoder(w).Encode(upstream.Daily{Summary: upstream.Summary{Period: "today", Totals: &upstream.Totals{Costs: []upstream.Cost{}}, Models: []upstream.Model{}}, Days: []upstream.Day{}})
		}
	}))
	t.Cleanup(srv.Close)
	c := upstream.New(5*time.Second, 2)
	t.Cleanup(c.Close)
	s := &Service{Discovery: staticDiscovery{{Name: "alice", URL: srv.URL, Source: "yaml"}}, Upstream: c}

	ctx, cancel := context.WithCancel(context.Background())
	canceled, waiting := make(chan Response, 1), make(chan Response, 1)
	go func() {
		out, _ := s.Dashboard(ctx, "today", "")
		canceled <- out
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("snapshot refresh did not start")
	}
	go func() {
		out, _ := s.Dashboard(context.Background(), "today", "")
		waiting <- out
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case out := <-canceled:
		if len(out.Data.Accounts) != 0 || len(out.Errors) != 1 || out.Errors[0].Message != "request canceled" {
			t.Fatalf("canceled request: %+v", out)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled request did not return")
	}
	close(release)
	var shared Response
	select {
	case shared = <-waiting:
	case <-time.After(time.Second):
		t.Fatal("waiting request did not receive the shared snapshot")
	}
	stored, _ := s.Dashboard(context.Background(), "today", "")
	// The round confirms the login before and after its reads, so it calls /usage twice.
	for _, out := range []Response{shared, stored} {
		if len(out.Errors) != 0 || len(out.Data.Accounts) != 1 || usageCalls.Load() != 2 {
			t.Fatalf("shared snapshot was canceled or replaced: usage=%d out=%+v", usageCalls.Load(), out)
		}
	}
}

// accountEndpoint serves usage that belongs to whichever account login holds when a request arrives.
// onUsage, when set, runs before /usage responds; returning false fails the request.
func accountEndpoint(t *testing.T, name string, login *atomic.Value, onUsage func() bool) discovery.Endpoint {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := login.Load().(string)
		tokens := map[string]int64{"Alice": 100, "Bob": 200}[current]
		period := r.URL.Query().Get("period")
		totals := upstream.Totals{Tokens: tokens, Costs: []upstream.Cost{}}
		summary := upstream.Summary{Period: period, Totals: &totals, Models: []upstream.Model{}}
		switch r.URL.Path {
		case "/usage":
			if onUsage != nil && !onUsage() {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(w).Encode(upstream.Usage{Login: current})
		case "/token-usage/daily":
			_ = json.NewEncoder(w).Encode(upstream.Daily{Summary: summary, Days: []upstream.Day{}})
		case "/token-usage/events":
			_ = json.NewEncoder(w).Encode(upstream.Events{Items: []upstream.Event{{ID: 1, UserID: current, Tokens: tokens}}, Period: period, Page: 1, PageSize: 20, Total: 1, TotalPages: 1})
		}
	}))
	t.Cleanup(srv.Close)
	return discovery.Endpoint{Name: name, URL: srv.URL, Source: "yaml"}
}

func TestEndpointAccountChangeIsNotAttributedToPreviousLogin(t *testing.T) {
	for _, mode := range []string{"live", "persistent"} {
		t.Run(mode, func(t *testing.T) {
			var login atomic.Value
			login.Store("Alice")
			c := upstream.New(time.Second, 4)
			t.Cleanup(c.Close)
			s := &Service{Discovery: staticDiscovery{accountEndpoint(t, "e", &login, nil)}, Upstream: c}
			if mode == "persistent" {
				s.History = &memoryHistory{hasSnapshot: true, days: []upstream.Day{}}
			}
			_, _ = s.Dashboard(context.Background(), "today", "")
			login.Store("Bob")
			stored, found := s.Dashboard(context.Background(), "today", "Alice")
			if !found || (mode == "live" && (stored.Data.Totals == nil || stored.Data.Totals.Tokens != 100)) {
				t.Fatalf("usage reported after the change was attributed to the previous login: %+v", stored.Data.Totals)
			}
			if events, status := s.Events(context.Background(), "Alice", "today", 1, 20); status != 404 || events.Data != nil {
				t.Fatalf("events of the new account were attributed to the previous login: %d %+v", status, events.Data)
			}
			out, found := s.Dashboard(context.Background(), "today", "Bob")
			if !found || len(out.Data.Accounts) != 1 || (mode == "live" && (out.Data.Totals == nil || out.Data.Totals.Tokens != 200)) {
				t.Fatalf("snapshot was not replaced after the change: %+v", out.Data)
			}
		})
	}
}

func TestEventsFollowMovedAccountAndRequireConfirmation(t *testing.T) {
	var first, second atomic.Value
	first.Store("Alice")
	second.Store("Bob")
	var confirm atomic.Bool
	confirm.Store(true)
	c := upstream.New(time.Second, 4)
	t.Cleanup(c.Close)
	s := &Service{Discovery: staticDiscovery{accountEndpoint(t, "e1", &first, confirm.Load), accountEndpoint(t, "e2", &second, nil)}, Upstream: c}
	_, _ = s.Dashboard(context.Background(), "today", "")
	first.Store("Bob")
	second.Store("Alice")
	events, status := s.Events(context.Background(), "Alice", "today", 1, 20)
	if status != 200 || events.Data.Items[0].UserID != "Alice" {
		t.Fatalf("events did not follow the moved account: %d %+v", status, events)
	}
	confirm.Store(false)
	events, status = s.Events(context.Background(), "Bob", "today", 1, 20)
	if status != 502 || events.Data != nil || len(events.Errors) != 1 || events.Errors[0].Operation != "usage" {
		t.Fatalf("events were returned for an unconfirmed account: %d %+v", status, events)
	}
}

func TestAccountChangeDuringRunningRoundTakesNewerRound(t *testing.T) {
	var login atomic.Value
	login.Store("Alice")
	var armed atomic.Bool
	var confirmations atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	hold := func() bool {
		if armed.CompareAndSwap(true, false) {
			close(started)
			<-release
			return true
		}
		confirmations.Add(1)
		return true
	}
	c := upstream.New(5*time.Second, 4)
	t.Cleanup(c.Close)
	s := &Service{Discovery: staticDiscovery{accountEndpoint(t, "e", &login, hold)}, Upstream: c}
	_, _ = s.Dashboard(context.Background(), "today", "")
	// Hold the next round's confirmation, which reads the login before the change.
	armed.Store(true)
	synced := make(chan struct{})
	go func() {
		defer close(synced)
		_, _ = s.Sync(context.Background(), "")
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("round did not start")
	}
	login.Store("Bob")
	before := confirmations.Load()
	result := make(chan int, 1)
	go func() {
		_, status := s.Events(context.Background(), "Alice", "today", 1, 20)
		result <- status
	}()
	for deadline := time.Now().Add(time.Second); confirmations.Load() == before; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("events did not confirm the account")
		}
	}
	// Let the events request join the running round, which saw the account before the change.
	time.Sleep(20 * time.Millisecond)
	close(release)
	select {
	case status := <-result:
		if status != 404 {
			t.Fatalf("events used a round that predates the account change: status=%d", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("events did not return")
	}
	<-synced
}

func TestAccountChangeDuringReadsIsNotAttributed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		switches []string // logins that the endpoint takes after each later batch of token reads
		want     string
		tokens   int64 // tokens of the listed account, or -1 when its usage is unavailable
	}{
		{name: "once", switches: []string{"Bob"}, want: "Bob", tokens: 200},
		{name: "twice", switches: []string{"Bob", "Carol"}, want: "Carol", tokens: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var login atomic.Value
			login.Store("Alice")
			var mu sync.Mutex
			var pending []string
			served := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/usage":
					_ = json.NewEncoder(w).Encode(upstream.Usage{Login: login.Load().(string)})
				case "/token-usage/daily":
					tokens := map[string]int64{"Alice": 100, "Bob": 200, "Carol": 300}[login.Load().(string)]
					totals := upstream.Totals{Tokens: tokens, Costs: []upstream.Cost{}}
					_ = json.NewEncoder(w).Encode(upstream.Daily{Summary: upstream.Summary{Period: r.URL.Query().Get("period"), Totals: &totals, Models: []upstream.Model{}}, Days: []upstream.Day{}})
					mu.Lock()
					defer mu.Unlock()
					if served++; served%len(upstream.Periods) == 0 && len(pending) > 0 {
						login.Store(pending[0])
						pending = pending[1:]
					}
				}
			}))
			t.Cleanup(srv.Close)
			c := upstream.New(time.Second, 16)
			t.Cleanup(c.Close)
			s := &Service{Discovery: staticDiscovery{{Name: "e", URL: srv.URL, Source: "yaml"}}, Upstream: c}
			_, _ = s.Dashboard(context.Background(), "today", "")
			mu.Lock()
			pending = tc.switches
			mu.Unlock()
			failures, _ := s.Sync(context.Background(), "")
			out, _ := s.Dashboard(context.Background(), "today", "")
			if len(out.Data.Accounts) != 1 || out.Data.Accounts[0].Login != tc.want {
				t.Fatalf("listed accounts: %+v", out.Data.Accounts)
			}
			if totals := out.Data.Accounts[0].Totals; tc.tokens < 0 {
				changed := discovery.Failure{Target: "e", Operation: "token-usage/daily", Message: errAccountChanged.Error()}
				if totals != nil || !slices.Contains(out.Errors, changed) || !slices.Contains(failures, changed) {
					t.Fatalf("usage read across account changes was kept: totals=%+v errors=%+v failures=%+v", totals, out.Errors, failures)
				}
			} else if totals == nil || totals.Tokens != tc.tokens || len(out.Errors) != 0 || len(failures) != 0 {
				t.Fatalf("usage of %s: totals=%+v errors=%+v failures=%+v", tc.want, totals, out.Errors, failures)
			}
		})
	}
}

func TestSyncReportsTokenUsageFailuresOfSelectedAccounts(t *testing.T) {
	a, b := testTarget(t, "Alice", 100, "/token-usage/daily"), testTarget(t, "Bob", 200, "")
	s := serviceFor(t, a, b)
	failures, found := s.Sync(context.Background(), "")
	want := []discovery.Failure{{Target: "Alice", Operation: "token-usage/daily", Message: "upstream returned HTTP 503"}}
	if !found || !reflect.DeepEqual(failures, want) {
		t.Fatalf("token usage failures: %+v", failures)
	}
	if failures, found = s.Sync(context.Background(), "bob"); !found || len(failures) != 0 {
		t.Fatalf("targeted sync reported other accounts: %+v", failures)
	}
}

func TestHistoryIsNotSavedWhenAccountChangesDuringRead(t *testing.T) {
	var login atomic.Value
	login.Store("Alice")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/usage":
			_ = json.NewEncoder(w).Encode(upstream.Usage{Login: login.Load().(string)})
		case "/token-usage/daily":
			totals := upstream.Totals{Tokens: 100, Costs: []upstream.Cost{}}
			_ = json.NewEncoder(w).Encode(upstream.Daily{Summary: upstream.Summary{Period: "lifetime", Totals: &totals, Models: []upstream.Model{}}, Days: []upstream.Day{}})
			login.Store("Bob")
		}
	}))
	t.Cleanup(srv.Close)
	c := upstream.New(time.Second, 4)
	t.Cleanup(c.Close)
	history := &memoryHistory{}
	s := &Service{Discovery: staticDiscovery{{Name: "e", URL: srv.URL, Source: "yaml"}}, Upstream: c, History: history}
	failures, _ := s.Sync(context.Background(), "")
	want := []discovery.Failure{{Target: "e", Operation: "token-usage/daily", Message: errAccountChanged.Error()}}
	if history.saveCalls != 0 || !reflect.DeepEqual(failures, want) {
		t.Fatalf("history read across an account change: saves=%d failures=%+v", history.saveCalls, failures)
	}
}

func TestEventsAreConfirmedAfterTheyAreRead(t *testing.T) {
	var login atomic.Value
	login.Store("Alice")
	var armed atomic.Bool
	switched := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/usage":
			_ = json.NewEncoder(w).Encode(upstream.Usage{Login: login.Load().(string)})
			if armed.CompareAndSwap(true, false) {
				login.Store("Bob")
				close(switched)
			}
		case "/token-usage/events":
			// An events read that overlaps the confirmation is served after the account changed.
			select {
			case <-switched:
			case <-time.After(100 * time.Millisecond):
			}
			_ = json.NewEncoder(w).Encode(upstream.Events{Items: []upstream.Event{{ID: 1, UserID: login.Load().(string)}}, Period: r.URL.Query().Get("period"), Page: 1, PageSize: 20, Total: 1, TotalPages: 1})
		}
	}))
	t.Cleanup(srv.Close)
	c := upstream.New(time.Second, 4)
	t.Cleanup(c.Close)
	s := &Service{Discovery: staticDiscovery{{Name: "e", URL: srv.URL, Source: "yaml"}}, Upstream: c, History: &memoryHistory{hasSnapshot: true}}
	_, _ = s.Dashboard(context.Background(), "today", "")
	armed.Store(true)
	events, status := s.Events(context.Background(), "Alice", "today", 1, 20)
	if status != 200 || events.Data.Items[0].UserID != "Alice" {
		t.Fatalf("events read after the confirmation were returned: %d %+v", status, events)
	}
}

func TestPersistentDashboardReadsStoredAggregatesWithoutDailyRequest(t *testing.T) {
	target := testTarget(t, "Alice", 100, "")
	history := &memoryHistory{hasSnapshot: true, days: []upstream.Day{{
		Date:   "2026-09-08",
		Totals: &upstream.Totals{Tokens: 300, Requests: 2, Costs: []upstream.Cost{{Currency: "USD", Nanos: 9}}},
		Models: []upstream.Model{{Model: "model-a", Totals: upstream.Totals{Tokens: 300, Requests: 2, Costs: []upstream.Cost{{Currency: "USD", Nanos: 9}}}}},
	}}}
	s := serviceFor(t, target)
	s.History = history
	s.Now = func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.Local) }
	out, found := s.Dashboard(context.Background(), "lifetime", "Alice")
	if !found || len(out.Errors) != 0 || out.Data.Totals.Tokens != 300 || out.Data.Accounts[0].Totals.Tokens != 300 || out.Data.Models[0].Tokens != 300 {
		t.Fatalf("persisted dashboard: %+v", out)
	}
	if target.count("/token-usage/daily") != 0 || history.saveCalls != 0 || history.loadCalls != 1 || history.period != "lifetime" {
		t.Fatalf("unexpected persistence calls: target=%+v history=%+v", target.calls, history)
	}
	if _, status := s.Events(context.Background(), "Alice", "today", 2, 1); status != 200 || history.saveCalls != 0 || history.loadCalls != 1 {
		t.Fatal("events touched persistence")
	}
}

func TestPersistentDashboardServesLastStoredDataWithoutDailyRequest(t *testing.T) {
	target := testTarget(t, "Alice", 100, "/token-usage/daily")
	history := &memoryHistory{hasSnapshot: true, days: []upstream.Day{{Date: "2026-09-08", Totals: &upstream.Totals{Tokens: 999}, Models: []upstream.Model{}}}}
	s := serviceFor(t, target)
	s.History = history
	out, found := s.Dashboard(context.Background(), "lifetime", "Alice")
	if !found || out.Data.Totals.Tokens != 999 || out.Data.Accounts[0].Totals.Tokens != 999 || len(out.Errors) != 0 || history.saveCalls != 0 || history.loadCalls != 1 || target.count("/token-usage/daily") != 0 {
		t.Fatalf("stored data not served: %+v", out)
	}
}

func TestPersistentDashboardDistinguishesMissingAndEmptySnapshots(t *testing.T) {
	target := testTarget(t, "Alice", 100, "")

	missing := serviceFor(t, target)
	missing.History = &memoryHistory{}
	out, found := missing.Dashboard(context.Background(), "lifetime", "Alice")
	if !found || out.Data.Totals != nil || out.Data.Accounts[0].Totals != nil || len(out.Errors) != 1 || out.Errors[0].Message != "usage has not been synchronized" {
		t.Fatalf("missing snapshot: %+v", out)
	}

	empty := serviceFor(t, target)
	empty.History = &memoryHistory{hasSnapshot: true, days: []upstream.Day{}}
	out, found = empty.Dashboard(context.Background(), "lifetime", "Alice")
	if !found || out.Data.Totals == nil || out.Data.Totals.Tokens != 0 || out.Data.Accounts[0].Totals == nil || out.Data.Accounts[0].Totals.Tokens != 0 || len(out.Data.Models) != 0 || len(out.Data.Days) != 0 || len(out.Errors) != 0 {
		t.Fatalf("empty snapshot: %+v", out)
	}
}

func TestSyncFetchesLifetimeAndCanTargetOneAccount(t *testing.T) {
	a, b := testTarget(t, "Alice", 100, ""), testTarget(t, "Bob", 200, "")
	history := &memoryHistory{}
	s := serviceFor(t, a, b)
	s.History = history
	failures, found := s.Sync(context.Background(), "alice")
	if !found || len(failures) != 0 || a.count("/token-usage/daily") != 1 || b.count("/token-usage/daily") != 0 || history.saveCalls != 1 || history.period != "lifetime" {
		t.Fatalf("targeted sync: found=%v failures=%+v history=%+v", found, failures, history)
	}
	failures, found = s.Sync(context.Background(), "missing")
	if found || len(failures) != 1 || failures[0].Operation != "account" || history.saveCalls != 1 {
		t.Fatalf("missing account sync: found=%v failures=%+v", found, failures)
	}
	failures, found = s.Sync(context.Background(), "alice")
	if !found || len(failures) != 0 || a.count("/token-usage/daily") != 2 || history.saveCalls != 2 {
		t.Fatalf("completed sync was retained: found=%v failures=%+v history=%+v", found, failures, history)
	}
}

func TestConcurrentSyncsShareOneAccountTask(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var startOnce sync.Once
	target := testTarget(t, "Alice", 100, "")
	original := target.endpoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target.mu.Lock()
		target.calls[r.URL.Path]++
		target.mu.Unlock()
		if r.URL.Path == "/usage" {
			_ = json.NewEncoder(w).Encode(upstream.Usage{Login: "Alice"})
			return
		}
		if r.URL.Path == "/token-usage/daily" {
			startOnce.Do(func() { close(started) })
			<-release
			totals := upstream.Totals{Tokens: 100, Costs: []upstream.Cost{}}
			_ = json.NewEncoder(w).Encode(upstream.Daily{Summary: upstream.Summary{Period: "lifetime", Totals: &totals, Models: []upstream.Model{}}, Days: []upstream.Day{}})
		}
	}))
	t.Cleanup(srv.Close)
	target.endpoint = discovery.Endpoint{Name: original.Name, URL: srv.URL, Source: original.Source}
	history := &synchronizedHistory{}
	s := serviceFor(t, target)
	s.History = history

	const callers = 20
	ready, begin := make(chan struct{}, callers), make(chan struct{})
	results := make(chan []discovery.Failure, callers)
	for range callers {
		go func() {
			ready <- struct{}{}
			<-begin
			failures, _ := s.Sync(context.Background(), "Alice")
			results <- failures
		}()
	}
	for range callers {
		<-ready
	}
	close(begin)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("account sync did not start")
	}
	time.Sleep(10 * time.Millisecond)
	close(release)
	for range callers {
		if failures := <-results; len(failures) != 0 {
			t.Fatalf("shared sync failed: %+v", failures)
		}
	}
	if got := target.count("/token-usage/daily"); got != 1 || history.count("Alice") != 1 {
		t.Fatalf("account task was not shared: daily=%d saves=%d", got, history.count("Alice"))
	}
}

func TestSyncAccountKeyIncludesTaskIdentity(t *testing.T) {
	base := resolved{
		endpoint: discovery.Endpoint{Source: "yaml", Name: "account-a", URL: "http://example", APIKey: "secret-key"},
		usage:    &upstream.Usage{Login: "Alice"},
	}
	baseKey := syncAccountKey(base)
	if strings.Contains(baseKey, base.endpoint.APIKey) {
		t.Fatal("sync account key contains the API key")
	}

	tests := []struct {
		name   string
		change func(*resolved)
	}{
		{name: "source", change: func(account *resolved) { account.endpoint.Source = "docker" }},
		{name: "name", change: func(account *resolved) { account.endpoint.Name = "account-b" }},
		{name: "URL", change: func(account *resolved) { account.endpoint.URL = "http://other" }},
		{name: "credential", change: func(account *resolved) { account.endpoint.APIKey = "new-secret-key" }},
		{name: "login", change: func(account *resolved) { account.usage = &upstream.Usage{Login: "Bob"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := base
			test.change(&changed)
			if syncAccountKey(changed) == baseKey {
				t.Fatalf("sync account key did not include %s", test.name)
			}
		})
	}

	caseVariant := base
	caseVariant.usage = &upstream.Usage{Login: "alice"}
	if syncAccountKey(caseVariant) != baseKey {
		t.Fatal("login casing changed the sync account key")
	}
}

func TestCredentialChangeDoesNotJoinRunningAccountTask(t *testing.T) {
	oldStarted, newStarted, releaseOld := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-releaseOld:
		default:
			close(releaseOld)
		}
	}()
	var oldOnce, newOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/usage" {
			_ = json.NewEncoder(w).Encode(upstream.Usage{Login: "Alice"})
			return
		}
		if r.URL.Path != "/token-usage/daily" {
			http.NotFound(w, r)
			return
		}
		switch r.Header.Get("x-api-key") {
		case "old-key":
			oldOnce.Do(func() { close(oldStarted) })
			<-releaseOld
		case "new-key":
			newOnce.Do(func() { close(newStarted) })
		default:
			http.Error(w, "unexpected credential", http.StatusUnauthorized)
			return
		}
		totals := upstream.Totals{Tokens: 100, Costs: []upstream.Cost{}}
		_ = json.NewEncoder(w).Encode(upstream.Daily{Summary: upstream.Summary{Period: "lifetime", Totals: &totals, Models: []upstream.Model{}}, Days: []upstream.Day{}})
	}))
	t.Cleanup(srv.Close)

	var endpointMu sync.RWMutex
	endpoint := discovery.Endpoint{Source: "yaml", Name: "account-a", URL: srv.URL, APIKey: "old-key"}
	discover := discoveryFunc(func(context.Context) ([]discovery.Endpoint, []discovery.Failure) {
		endpointMu.RLock()
		defer endpointMu.RUnlock()
		return []discovery.Endpoint{endpoint}, nil
	})
	client := upstream.New(time.Second, 4)
	t.Cleanup(client.Close)
	s := &Service{Discovery: discover, Upstream: client, History: &synchronizedHistory{}}

	results := make(chan []discovery.Failure, 2)
	go func() {
		failures, _ := s.Sync(context.Background(), "Alice")
		results <- failures
	}()
	select {
	case <-oldStarted:
	case <-time.After(time.Second):
		t.Fatal("old credential request did not start")
	}

	endpointMu.Lock()
	endpoint.APIKey = "new-key"
	endpointMu.Unlock()
	go func() {
		failures, _ := s.Sync(context.Background(), "Alice")
		results <- failures
	}()
	select {
	case <-newStarted:
	case <-time.After(time.Second):
		t.Fatal("new credential request joined the running old credential task")
	}
	close(releaseOld)
	for range 2 {
		if failures := <-results; len(failures) != 0 {
			t.Fatalf("sync failed: %+v", failures)
		}
	}
}

func TestDifferentAccountsSyncConcurrently(t *testing.T) {
	var active, peak atomic.Int32
	secondStarted := make(chan struct{})
	var secondOnce sync.Once
	makeTarget := func(login string) *target {
		target := testTarget(t, login, 100, "")
		original := target.endpoint
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			target.mu.Lock()
			target.calls[r.URL.Path]++
			target.mu.Unlock()
			if r.URL.Path == "/usage" {
				_ = json.NewEncoder(w).Encode(upstream.Usage{Login: login})
				return
			}
			if r.URL.Path == "/token-usage/daily" {
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
				if n == 2 {
					secondOnce.Do(func() { close(secondStarted) })
				}
				select {
				case <-secondStarted:
				case <-time.After(500 * time.Millisecond):
				}
				totals := upstream.Totals{Tokens: 100, Costs: []upstream.Cost{}}
				_ = json.NewEncoder(w).Encode(upstream.Daily{Summary: upstream.Summary{Period: "lifetime", Totals: &totals, Models: []upstream.Model{}}, Days: []upstream.Day{}})
			}
		}))
		t.Cleanup(srv.Close)
		target.endpoint = discovery.Endpoint{Name: original.Name, URL: srv.URL, Source: original.Source}
		return target
	}
	a, b := makeTarget("Alice"), makeTarget("Bob")
	s := serviceFor(t, a, b)
	s.History = &synchronizedHistory{}
	begin := make(chan struct{})
	results := make(chan []discovery.Failure, 2)
	for _, login := range []string{"Alice", "Bob"} {
		go func() {
			<-begin
			failures, _ := s.Sync(context.Background(), login)
			results <- failures
		}()
	}
	close(begin)
	for range 2 {
		if failures := <-results; len(failures) != 0 {
			t.Fatalf("concurrent sync failed: %+v", failures)
		}
	}
	if peak.Load() != 2 {
		t.Fatalf("different accounts remained serialized: peak=%d", peak.Load())
	}
}

func TestCanceledWaiterDoesNotCancelSharedAccountTask(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var startOnce sync.Once
	target := testTarget(t, "Alice", 100, "")
	original := target.endpoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target.mu.Lock()
		target.calls[r.URL.Path]++
		target.mu.Unlock()
		if r.URL.Path == "/usage" {
			_ = json.NewEncoder(w).Encode(upstream.Usage{Login: "Alice"})
			return
		}
		if r.URL.Path == "/token-usage/daily" {
			startOnce.Do(func() { close(started) })
			<-release
			totals := upstream.Totals{Tokens: 100, Costs: []upstream.Cost{}}
			_ = json.NewEncoder(w).Encode(upstream.Daily{Summary: upstream.Summary{Period: "lifetime", Totals: &totals, Models: []upstream.Model{}}, Days: []upstream.Day{}})
		}
	}))
	t.Cleanup(srv.Close)
	target.endpoint = discovery.Endpoint{Name: original.Name, URL: srv.URL, Source: original.Source}
	history := &synchronizedHistory{}
	s := serviceFor(t, target)
	s.History = history

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan []discovery.Failure, 1)
	go func() {
		failures, _ := s.Sync(firstCtx, "Alice")
		firstResult <- failures
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("account sync did not start")
	}
	secondStarted := make(chan struct{})
	secondResult := make(chan []discovery.Failure, 1)
	go func() {
		close(secondStarted)
		failures, _ := s.Sync(context.Background(), "Alice")
		secondResult <- failures
	}()
	<-secondStarted
	time.Sleep(10 * time.Millisecond)
	cancelFirst()
	select {
	case failures := <-firstResult:
		if len(failures) != 1 || failures[0].Message != "request canceled" {
			t.Fatalf("canceled waiter result: %+v", failures)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter did not return")
	}
	close(release)
	select {
	case failures := <-secondResult:
		if len(failures) != 0 {
			t.Fatalf("shared task was canceled: %+v", failures)
		}
	case <-time.After(time.Second):
		t.Fatal("remaining waiter did not finish")
	}
	if target.count("/token-usage/daily") != 1 || history.count("Alice") != 1 {
		t.Fatal("shared account task was repeated or not persisted")
	}
}

func TestWaitForSyncDrainsAndStopsSharedTasks(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	target := testTarget(t, "Alice", 100, "")
	original := target.endpoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/usage" {
			_ = json.NewEncoder(w).Encode(upstream.Usage{Login: "Alice"})
			return
		}
		if r.URL.Path == "/token-usage/daily" {
			close(started)
			<-release
			totals := upstream.Totals{Costs: []upstream.Cost{}}
			_ = json.NewEncoder(w).Encode(upstream.Daily{Summary: upstream.Summary{Period: "lifetime", Totals: &totals, Models: []upstream.Model{}}, Days: []upstream.Day{}})
		}
	}))
	t.Cleanup(srv.Close)
	target.endpoint = discovery.Endpoint{Name: original.Name, URL: srv.URL, Source: original.Source}
	s := serviceFor(t, target)
	s.History = &synchronizedHistory{}
	syncResult := make(chan []discovery.Failure, 1)
	go func() {
		failures, _ := s.Sync(context.Background(), "Alice")
		syncResult <- failures
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("account sync did not start")
	}
	drained := make(chan struct{})
	go func() {
		s.WaitForSync()
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("active account sync was not drained")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("sync drain did not finish")
	}
	if failures := <-syncResult; len(failures) != 0 {
		t.Fatalf("drained sync failed: %+v", failures)
	}
	failures, _ := s.Sync(context.Background(), "Alice")
	if len(failures) != 1 || failures[0].Message != "request canceled" {
		t.Fatalf("new sync started after drain: %+v", failures)
	}
}
