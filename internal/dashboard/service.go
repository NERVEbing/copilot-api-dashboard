package dashboard

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/NERVEbing/copilot-api-dashboard/internal/discovery"
	"github.com/NERVEbing/copilot-api-dashboard/internal/upstream"
)

type Discovery interface {
	Discover(context.Context) ([]discovery.Endpoint, []discovery.Failure)
}

type Service struct {
	Discovery Discovery
	Upstream  *upstream.Client
}

type Account struct {
	upstream.Usage
	Totals *upstream.Totals `json:"totals"`
}

type Day struct {
	Date   string           `json:"date"`
	Totals upstream.Totals  `json:"totals"`
	Models []upstream.Model `json:"by_model"`
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
	return out, found
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
