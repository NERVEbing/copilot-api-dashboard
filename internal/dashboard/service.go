package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	Discovery   Discovery
	Upstream    *upstream.Client
	History     History
	Now         func() time.Time
	SyncContext context.Context
	resolveSync singleflight.Group
	accountSync singleflight.Group
	syncTaskMu  sync.Mutex
	syncTasks   int
	syncStopped bool
	syncDone    chan struct{}
}

type History interface {
	SaveDaily(context.Context, string, string, string, string, *upstream.Daily) error
	LoadDaily(context.Context, string, string, string, string, time.Time) ([]upstream.Day, bool, error)
}

type Account struct {
	upstream.Usage
	Totals *upstream.Totals `json:"totals"`
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
}

type syncResolution struct {
	accounts []resolved
	failures []discovery.Failure
}

type accountSyncResult struct {
	failure *discovery.Failure
}

func failure(e discovery.Endpoint, operation string, err error) discovery.Failure {
	return discovery.Failure{Target: e.Name, Operation: operation, Message: err.Error()}
}

func (s *Service) resolve(ctx context.Context) ([]resolved, []discovery.Failure) {
	endpoints, failures := s.Discovery.Discover(ctx)
	if failures == nil {
		failures = []discovery.Failure{}
	}
	endpoints = discovery.Deduplicate(endpoints)
	results := make([]resolved, len(endpoints))
	errs := make([]error, len(endpoints))
	var wg sync.WaitGroup
	for i, e := range endpoints {
		wg.Go(func() { results[i].endpoint = e; results[i].usage, errs[i] = s.Upstream.Usage(ctx, e) })
	}
	wg.Wait()
	accounts := []resolved{}
	seen := map[string]bool{}
	for i, result := range results {
		if errs[i] != nil {
			failures = append(failures, failure(result.endpoint, "usage", errs[i]))
			continue
		}
		key := strings.ToLower(result.usage.Login)
		if !seen[key] {
			seen[key] = true
			accounts = append(accounts, result)
		}
	}
	sort.Slice(accounts, func(i, j int) bool {
		return strings.ToLower(accounts[i].usage.Login) < strings.ToLower(accounts[j].usage.Login)
	})
	return accounts, failures
}

func (s *Service) Dashboard(ctx context.Context, period, login string) (Response, bool) {
	accounts, failures := s.resolve(ctx)
	if s.History != nil {
		return s.persistedDashboard(ctx, accounts, failures, period, login)
	}
	out := Response{Data: Data{Period: period, Accounts: []Account{}}, Errors: failures}
	found := login == ""
	if login != "" {
		out.Data.SelectedAccount = &login
	}
	type fetched struct {
		summary              *upstream.Summary
		daily                *upstream.Daily
		summaryErr, dailyErr error
	}
	results := make([]fetched, len(accounts))
	var wg sync.WaitGroup
	for i, a := range accounts {
		out.Data.Accounts = append(out.Data.Accounts, Account{Usage: *a.usage})
		if login != "" && !strings.EqualFold(login, a.usage.Login) {
			continue
		}
		found = true
		if login != "" {
			canonical := a.usage.Login
			out.Data.SelectedAccount = &canonical
		}
		wg.Go(func() { results[i].summary, results[i].summaryErr = s.Upstream.Summary(ctx, a.endpoint, period) })
		wg.Go(func() { results[i].daily, results[i].dailyErr = s.Upstream.Daily(ctx, a.endpoint, period) })
	}
	wg.Wait()
	for i, result := range results {
		if result.summaryErr != nil {
			out.Errors = append(out.Errors, failure(accounts[i].endpoint, "token-usage", result.summaryErr))
		}
		if result.dailyErr != nil {
			out.Errors = append(out.Errors, failure(accounts[i].endpoint, "token-usage/daily", result.dailyErr))
		}
		if result.summary != nil {
			out.Data.Accounts[i].Totals = result.summary.Totals
			if out.Data.Totals == nil {
				out.Data.Totals = &upstream.Totals{Costs: []upstream.Cost{}}
				out.Data.Models = []upstream.Model{}
			}
			addTotals(out.Data.Totals, *result.summary.Totals)
			out.Data.Models = mergeModels(out.Data.Models, result.summary.Models)
		}
		if result.daily != nil {
			if out.Data.Days == nil {
				out.Data.Days = []Day{}
			}
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

func (s *Service) resolveForSync(ctx context.Context) ([]resolved, []discovery.Failure, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	result := s.resolveSync.DoChan("sync", func() (any, error) {
		if !s.beginSyncTask() {
			return nil, context.Canceled
		}
		defer s.finishSyncTask()
		accounts, failures := s.resolve(s.syncContext())
		return syncResolution{accounts: accounts, failures: failures}, nil
	})
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case value := <-result:
		if value.Err != nil {
			return nil, nil, value.Err
		}
		resolution := value.Val.(syncResolution)
		return resolution.accounts, append([]discovery.Failure(nil), resolution.failures...), nil
	}
}

func syncAccountKey(account resolved) string {
	credential := sha256.Sum256([]byte(account.endpoint.APIKey))
	return strings.Join([]string{
		account.endpoint.Source,
		account.endpoint.Name,
		account.endpoint.URL,
		strings.ToLower(account.usage.Login),
		hex.EncodeToString(credential[:]),
	}, "\x00")
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

func (s *Service) persistedDashboard(ctx context.Context, accounts []resolved, failures []discovery.Failure, period, login string) (Response, bool) {
	out := Response{Data: Data{Period: period, Accounts: []Account{}}, Errors: failures}
	found := login == ""
	if login != "" {
		out.Data.SelectedAccount = &login
	}
	for i, account := range accounts {
		out.Data.Accounts = append(out.Data.Accounts, Account{Usage: *account.usage})
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

func (s *Service) Sync(ctx context.Context, login string) ([]discovery.Failure, bool) {
	if s.History == nil {
		return []discovery.Failure{}, true
	}
	accounts, failures, err := s.resolveForSync(ctx)
	if err != nil {
		return []discovery.Failure{{Target: "sync", Operation: "discovery", Message: discovery.SafeMessage(err)}}, login == ""
	}
	found := login == ""
	results := make([]*discovery.Failure, len(accounts))
	var wg sync.WaitGroup
	for i, account := range accounts {
		if login != "" && !strings.EqualFold(login, account.usage.Login) {
			continue
		}
		found = true
		wg.Go(func() { results[i] = s.syncAccount(ctx, account) })
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

func (s *Service) Events(ctx context.Context, login, period string, page, size int) (EventsResponse, int) {
	accounts, failures := s.resolve(ctx)
	out := EventsResponse{Errors: failures}
	for _, a := range accounts {
		if !strings.EqualFold(login, a.usage.Login) {
			continue
		}
		data, err := s.Upstream.Events(ctx, a.endpoint, period, page, size)
		if err != nil {
			out.Errors = append(out.Errors, failure(a.endpoint, "token-usage/events", err))
			return out, 502
		}
		out.Data = data
		return out, 200
	}
	out.Errors = append(out.Errors, discovery.Failure{Target: login, Operation: "account", Message: "account unavailable in current discovery"})
	return out, 404
}
