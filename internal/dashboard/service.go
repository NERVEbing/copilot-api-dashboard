package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/NERVEbing/copilot-api-dashboard/internal/discovery"
	"github.com/NERVEbing/copilot-api-dashboard/internal/upstream"
)

type Discovery interface {
	Discover(context.Context) ([]discovery.Endpoint, []discovery.Failure)
}

type Service struct {
	Discovery      Discovery
	Upstream       *upstream.Client
	History        History
	AccountAliases map[string]string
	Now            func() time.Time
	SyncContext    context.Context
	resolveSync    singleflight.Group
	accountSync    singleflight.Group
	snapshotMu     sync.Mutex
	snapshot       *accountSnapshot
	syncTaskMu     sync.Mutex
	syncTasks      int
	syncStopped    bool
	syncDone       chan struct{}
}

type History interface {
	SaveDaily(context.Context, string, string, string, string, *upstream.Daily) error
	LoadDaily(context.Context, string, string, string, string, time.Time) ([]upstream.Day, bool, error)
}

type Account struct {
	upstream.Usage
	DisplayName string           `json:"display_name,omitempty"`
	Totals      *upstream.Totals `json:"totals"`
}

type Day struct {
	Date     string           `json:"date"`
	Recorded bool             `json:"recorded"`
	Totals   upstream.Totals  `json:"totals"`
	Models   []upstream.Model `json:"by_model"`
}

type Data struct {
	Period          string           `json:"period"`
	SelectedAccount *string          `json:"selected_account"`
	Accounts        []Account        `json:"accounts"`
	UpdatedAtMS     *int64           `json:"updated_at_ms"`
	Totals          *upstream.Totals `json:"totals"`
	Models          []upstream.Model `json:"by_model"`
	Days            []Day            `json:"days"`
}

type Response struct {
	Data   Data                `json:"data"`
	Errors []discovery.Failure `json:"errors"`
}

type EventsResponse struct {
	Data   *upstream.Events    `json:"data"`
	Errors []discovery.Failure `json:"errors"`
}

type resolved struct {
	endpoint discovery.Endpoint
	usage    *upstream.Usage
	// periods holds token usage by period when persistence is disabled.
	periods map[string]*periodUsage
}

type periodUsage struct {
	daily *upstream.Daily
	err   error
}

// accountSnapshot is one complete round of discovery, /usage, and, without persistence, token usage.
// Token usage is kept only for the login that its endpoint reported both before and after the reads.
// A snapshot is never modified after it is stored.
type accountSnapshot struct {
	accounts []resolved
	failures []discovery.Failure
	// observed holds the login that each endpoint reported last in the round, keyed by endpointKey.
	observed  map[string]string
	updatedAt time.Time
}

var errAccountChanged = errors.New("account changed while data was read")

type accountSyncResult struct {
	failure *discovery.Failure
}

func failure(e discovery.Endpoint, operation string, err error) discovery.Failure {
	return discovery.Failure{Target: e.Name, Operation: operation, Message: err.Error()}
}

// resolve discovers endpoints and reads their accounts. With tokens, it also reads token usage for every
// period, so that the dashboard can serve any period from the same round as the login. previous holds the
// logins observed by the previous round; the returned map holds the logins observed by this one.
func (s *Service) resolve(ctx context.Context, tokens bool, previous map[string]string) ([]resolved, []discovery.Failure, map[string]string) {
	endpoints, failures := s.Discovery.Discover(ctx)
	if failures == nil {
		failures = []discovery.Failure{}
	}
	endpoints = discovery.Deduplicate(endpoints)
	results := make([]resolved, len(endpoints))
	errs := make([]error, len(endpoints))
	var wg sync.WaitGroup
	for i, e := range endpoints {
		results[i].endpoint = e
		wg.Go(func() {
			if tokens {
				results[i].usage, results[i].periods, errs[i] = s.readEndpoint(ctx, e, previous[endpointKey(e)])
			} else {
				results[i].usage, errs[i] = s.Upstream.Usage(ctx, e)
			}
		})
	}
	wg.Wait()
	accounts := []resolved{}
	observed := map[string]string{}
	seen := map[string]bool{}
	for i, result := range results {
		if errs[i] != nil {
			failures = append(failures, failure(result.endpoint, "usage", errs[i]))
			continue
		}
		observed[endpointKey(result.endpoint)] = result.usage.Login
		key := strings.ToLower(result.usage.Login)
		if !seen[key] {
			seen[key] = true
			accounts = append(accounts, result)
		}
	}
	sort.Slice(accounts, func(i, j int) bool {
		return strings.ToLower(accounts[i].usage.Login) < strings.ToLower(accounts[j].usage.Login)
	})
	return accounts, failures, observed
}

// readEndpoint reads token usage for every period between two /usage calls that report the same login.
// The first of them is the login that the previous round observed last, or a new call when there is none.
// If the logins differ, the endpoint is read once more; a second change leaves its usage unavailable.
func (s *Service) readEndpoint(ctx context.Context, e discovery.Endpoint, before string) (*upstream.Usage, map[string]*periodUsage, error) {
	if before == "" {
		usage, err := s.Upstream.Usage(ctx, e)
		if err != nil {
			return nil, nil, err
		}
		before = usage.Login
	}
	for attempt := 0; ; attempt++ {
		periods := s.readPeriods(ctx, e)
		usage, err := s.Upstream.Usage(ctx, e)
		if err != nil {
			return nil, nil, err
		}
		if strings.EqualFold(usage.Login, before) {
			return usage, periods, nil
		}
		if attempt == 1 {
			for _, result := range periods {
				*result = periodUsage{err: errAccountChanged}
			}
			return usage, periods, nil
		}
		before = usage.Login
	}
}

func (s *Service) readPeriods(ctx context.Context, e discovery.Endpoint) map[string]*periodUsage {
	periods := make(map[string]*periodUsage, len(upstream.Periods))
	var wg sync.WaitGroup
	for _, period := range upstream.Periods {
		result := &periodUsage{}
		periods[period] = result
		wg.Go(func() { result.daily, result.err = s.Upstream.Daily(ctx, e, period) })
	}
	wg.Wait()
	return periods
}

func newResponse(current *accountSnapshot, period string) Response {
	out := Response{Data: Data{Period: period, Accounts: []Account{}}, Errors: append([]discovery.Failure{}, current.failures...)}
	if !current.updatedAt.IsZero() {
		updatedAt := current.updatedAt.UnixMilli()
		out.Data.UpdatedAtMS = &updatedAt
	}
	return out
}

func (current *accountSnapshot) find(login string) (resolved, bool) {
	for _, account := range current.accounts {
		if strings.EqualFold(login, account.usage.Login) {
			return account, true
		}
	}
	return resolved{}, false
}

func (s *Service) Dashboard(ctx context.Context, period, login string) (Response, bool) {
	current := s.latest(ctx)
	if s.History != nil {
		return s.persistedDashboard(ctx, current, period, login)
	}
	accounts := current.accounts
	out := newResponse(current, period)
	found := login == ""
	if login != "" {
		out.Data.SelectedAccount = &login
	}
	results := make([]periodUsage, len(accounts))
	for i, a := range accounts {
		out.Data.Accounts = append(out.Data.Accounts, Account{Usage: *a.usage, DisplayName: s.AccountAliases[strings.ToLower(a.usage.Login)]})
		if login != "" && !strings.EqualFold(login, a.usage.Login) {
			continue
		}
		found = true
		if login != "" {
			canonical := a.usage.Login
			out.Data.SelectedAccount = &canonical
		}
		if usage := a.periods[period]; usage != nil {
			results[i] = *usage
		}
	}
	for i, result := range results {
		if result.err != nil {
			out.Errors = append(out.Errors, failure(accounts[i].endpoint, "token-usage/daily", result.err))
		}
		if result.daily != nil {
			out.Data.Accounts[i].Totals = result.daily.Totals
			if out.Data.Totals == nil {
				out.Data.Totals = &upstream.Totals{Costs: []upstream.Cost{}}
				out.Data.Models = []upstream.Model{}
				out.Data.Days = []Day{}
			}
			addTotals(out.Data.Totals, *result.daily.Totals)
			out.Data.Models = mergeModels(out.Data.Models, result.daily.Models)
			for _, day := range result.daily.Days {
				out.Data.Days = mergeDay(out.Data.Days, day)
			}
		}
	}
	if !found {
		out.Errors = append(out.Errors, discovery.Failure{Target: login, Operation: "account", Message: "account unavailable in current discovery"})
	}
	out.Data.Days = fillDays(out.Data.Days, period, s.now())
	return out, found
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) syncContext() context.Context {
	if s.SyncContext != nil {
		return s.SyncContext
	}
	return context.Background()
}

func (s *Service) beginSyncTask() bool {
	s.syncTaskMu.Lock()
	defer s.syncTaskMu.Unlock()
	if s.syncStopped {
		return false
	}
	s.syncTasks++
	return true
}

func (s *Service) finishSyncTask() {
	s.syncTaskMu.Lock()
	defer s.syncTaskMu.Unlock()
	s.syncTasks--
	if s.syncStopped && s.syncTasks == 0 {
		close(s.syncDone)
	}
}

// WaitForSync prevents new shared synchronization tasks and waits for active tasks to finish.
func (s *Service) WaitForSync() {
	s.syncTaskMu.Lock()
	if !s.syncStopped {
		s.syncStopped = true
		s.syncDone = make(chan struct{})
		if s.syncTasks == 0 {
			close(s.syncDone)
		}
	}
	done := s.syncDone
	s.syncTaskMu.Unlock()
	<-done
}

func snapshotFailure(err error) discovery.Failure {
	return discovery.Failure{Target: "sync", Operation: "discovery", Message: discovery.SafeMessage(err)}
}

// refreshAccounts takes a new account snapshot. Concurrent callers share one refresh, which runs on
// the service context so that a canceled request can neither cut it short nor be stored as its result.
func (s *Service) refreshAccounts(ctx context.Context) (*accountSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := s.resolveSync.DoChan("accounts", func() (any, error) {
		if !s.beginSyncTask() {
			return nil, context.Canceled
		}
		defer s.finishSyncTask()
		taskCtx := s.syncContext()
		updatedAt := s.now()
		s.snapshotMu.Lock()
		var previous map[string]string
		if s.snapshot != nil {
			previous = s.snapshot.observed
		}
		s.snapshotMu.Unlock()
		accounts, failures, observed := s.resolve(taskCtx, s.History == nil, previous)
		if err := taskCtx.Err(); err != nil {
			return nil, err
		}
		next := &accountSnapshot{accounts: accounts, failures: failures, observed: observed, updatedAt: updatedAt}
		s.snapshotMu.Lock()
		s.snapshot = next
		s.snapshotMu.Unlock()
		return next, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case value := <-result:
		if value.Err != nil {
			return nil, value.Err
		}
		return value.Val.(*accountSnapshot), nil
	}
}

// latest returns the stored account snapshot and takes the first one when none exists.
func (s *Service) latest(ctx context.Context) *accountSnapshot {
	s.snapshotMu.Lock()
	current := s.snapshot
	s.snapshotMu.Unlock()
	if current != nil {
		return current
	}
	return s.refreshSince(ctx, time.Time{})
}

// refreshSince returns a snapshot whose round started no earlier than since. A round that was
// already running when a change was observed may have missed the change, so it takes one more.
func (s *Service) refreshSince(ctx context.Context, since time.Time) *accountSnapshot {
	current, err := s.refreshAccounts(ctx)
	if err == nil && current.updatedAt.Before(since) {
		current, err = s.refreshAccounts(ctx)
	}
	if err != nil {
		return &accountSnapshot{failures: []discovery.Failure{snapshotFailure(err)}}
	}
	return current
}

// endpointKey identifies an endpoint configuration without exposing its credential.
func endpointKey(e discovery.Endpoint) string {
	credential := sha256.Sum256([]byte(e.APIKey))
	return strings.Join([]string{e.Source, e.Name, e.URL, hex.EncodeToString(credential[:])}, "\x00")
}

func syncAccountKey(account resolved) string {
	return endpointKey(account.endpoint) + "\x00" + strings.ToLower(account.usage.Login)
}

func (s *Service) syncAccount(ctx context.Context, account resolved) *discovery.Failure {
	if err := ctx.Err(); err != nil {
		failure := failure(account.endpoint, "sync", err)
		failure.Message = discovery.SafeMessage(err)
		return &failure
	}
	result := s.accountSync.DoChan(syncAccountKey(account), func() (any, error) {
		if !s.beginSyncTask() {
			return nil, context.Canceled
		}
		defer s.finishSyncTask()
		taskCtx := s.syncContext()
		daily, err := s.Upstream.Daily(taskCtx, account.endpoint, "lifetime")
		if err != nil {
			value := failure(account.endpoint, "token-usage/daily", err)
			return accountSyncResult{failure: &value}, nil
		}
		// The snapshot observed the login before this read. Confirm it after the read, so that an endpoint
		// that now serves another account cannot store that account's usage under this login.
		usage, err := s.Upstream.Usage(taskCtx, account.endpoint)
		if err != nil {
			value := failure(account.endpoint, "usage", err)
			return accountSyncResult{failure: &value}, nil
		}
		if !strings.EqualFold(usage.Login, account.usage.Login) {
			value := failure(account.endpoint, "token-usage/daily", errAccountChanged)
			return accountSyncResult{failure: &value}, nil
		}
		if err := s.History.SaveDaily(taskCtx, account.endpoint.Source, account.endpoint.Name, account.endpoint.URL, account.usage.Login, daily); err != nil {
			value := failure(account.endpoint, "persistence", err)
			return accountSyncResult{failure: &value}, nil
		}
		return accountSyncResult{}, nil
	})
	select {
	case <-ctx.Done():
		value := failure(account.endpoint, "sync", ctx.Err())
		value.Message = discovery.SafeMessage(ctx.Err())
		return &value
	case value := <-result:
		if value.Err != nil {
			failure := failure(account.endpoint, "sync", value.Err)
			failure.Message = discovery.SafeMessage(value.Err)
			return &failure
		}
		return value.Val.(accountSyncResult).failure
	}
}

func summarizeDays(days []upstream.Day) (*upstream.Totals, []upstream.Model) {
	totals := &upstream.Totals{Costs: []upstream.Cost{}}
	models := []upstream.Model{}
	for _, day := range days {
		addTotals(totals, *day.Totals)
		models = mergeModels(models, day.Models)
	}
	return totals, models
}

func (s *Service) persistedDashboard(ctx context.Context, current *accountSnapshot, period, login string) (Response, bool) {
	out := newResponse(current, period)
	found := login == ""
	if login != "" {
		out.Data.SelectedAccount = &login
	}
	for i, account := range current.accounts {
		out.Data.Accounts = append(out.Data.Accounts, Account{Usage: *account.usage, DisplayName: s.AccountAliases[strings.ToLower(account.usage.Login)]})
		if login != "" && !strings.EqualFold(login, account.usage.Login) {
			continue
		}
		found = true
		if login != "" {
			canonical := account.usage.Login
			out.Data.SelectedAccount = &canonical
		}
		days, hasSnapshot, err := s.History.LoadDaily(ctx, account.endpoint.Name, account.endpoint.URL, account.usage.Login, period, s.now())
		if err != nil {
			out.Errors = append(out.Errors, failure(account.endpoint, "persistence", err))
			continue
		}
		if !hasSnapshot {
			out.Errors = append(out.Errors, discovery.Failure{Target: account.endpoint.Name, Operation: "persistence", Message: "usage has not been synchronized"})
			continue
		}
		accountTotals, accountModels := summarizeDays(days)
		out.Data.Accounts[i].Totals = accountTotals
		if out.Data.Totals == nil {
			out.Data.Totals = &upstream.Totals{Costs: []upstream.Cost{}}
			out.Data.Models = []upstream.Model{}
			out.Data.Days = []Day{}
		}
		addTotals(out.Data.Totals, *accountTotals)
		out.Data.Models = mergeModels(out.Data.Models, accountModels)
		for _, day := range days {
			out.Data.Days = mergeDay(out.Data.Days, day)
		}
	}
	if !found {
		out.Errors = append(out.Errors, discovery.Failure{Target: login, Operation: "account", Message: "account unavailable in current discovery"})
	}
	out.Data.Days = fillDays(out.Data.Days, period, s.now())
	return out, found
}

// Sync takes a new account snapshot and, with persistence, stores daily history for the selected accounts.
// History is always attributed with this fresh snapshot, never with a stored one. Without persistence,
// it reports the snapshot's token usage failures for the selected accounts.
func (s *Service) Sync(ctx context.Context, login string) ([]discovery.Failure, bool) {
	current, err := s.refreshAccounts(ctx)
	if err != nil {
		return []discovery.Failure{snapshotFailure(err)}, login == ""
	}
	accounts, failures := current.accounts, append([]discovery.Failure{}, current.failures...)
	found := login == ""
	results := make([]*discovery.Failure, len(accounts))
	var wg sync.WaitGroup
	for i, account := range accounts {
		if login != "" && !strings.EqualFold(login, account.usage.Login) {
			continue
		}
		found = true
		if s.History != nil {
			wg.Go(func() { results[i] = s.syncAccount(ctx, account) })
		}
		for _, period := range upstream.Periods {
			if usage := account.periods[period]; usage != nil && usage.err != nil {
				value := failure(account.endpoint, "token-usage/daily", usage.err)
				if !slices.Contains(failures, value) {
					failures = append(failures, value)
				}
			}
		}
	}
	wg.Wait()
	for i, result := range results {
		if login != "" && !strings.EqualFold(login, accounts[i].usage.Login) {
			continue
		}
		if result != nil {
			failures = append(failures, *result)
		}
	}
	if !found {
		failures = append(failures, discovery.Failure{Target: login, Operation: "account", Message: "account unavailable in current discovery"})
	}
	return failures, found
}

// Events reads request events live and returns them only when the endpoint still reports the selected
// account after the read, as it did when the snapshot was taken. If it reports another account, a newer
// snapshot locates the account and the read is repeated once.
func (s *Service) Events(ctx context.Context, login, period string, page, size int) (EventsResponse, int) {
	current := s.latest(ctx)
	for attempt := 0; ; attempt++ {
		out := EventsResponse{Errors: append([]discovery.Failure{}, current.failures...)}
		account, found := current.find(login)
		if !found {
			out.Errors = append(out.Errors, discovery.Failure{Target: login, Operation: "account", Message: "account unavailable in current discovery"})
			return out, 404
		}
		data, err := s.Upstream.Events(ctx, account.endpoint, period, page, size)
		if err != nil {
			out.Errors = append(out.Errors, failure(account.endpoint, "token-usage/events", err))
			return out, 502
		}
		usage, err := s.Upstream.Usage(ctx, account.endpoint)
		if err != nil {
			out.Errors = append(out.Errors, failure(account.endpoint, "usage", err))
			return out, 502
		}
		if strings.EqualFold(usage.Login, account.usage.Login) {
			out.Data = data
			return out, 200
		}
		if attempt == 1 {
			out.Errors = append(out.Errors, failure(account.endpoint, "token-usage/events", errAccountChanged))
			return out, 502
		}
		current = s.refreshSince(ctx, s.now())
	}
}
