package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/NERVEbing/copilot-api-dashboard/internal/discovery"
)

const MaxResponseBytes = 16 << 20

type Client struct {
	http    *http.Client
	limit   chan struct{}
	timeout time.Duration
}

func New(timeout time.Duration, concurrency int) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = concurrency
	return &Client{http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, limit: make(chan struct{}, concurrency), timeout: timeout}
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) get(ctx context.Context, e discovery.Endpoint, path string, query url.Values, dst any) error {
	select {
	case c.limit <- struct{}{}:
		defer func() { <-c.limit }()
	case <-ctx.Done():
		return errors.New(discovery.SafeMessage(ctx.Err()))
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	base, err := discovery.NormalizeURL(e.URL)
	if err != nil {
		return err
	}
	u, _ := url.Parse(base)
	u.Path, u.RawQuery = "/"+path, query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return errors.New("cannot construct upstream request")
	}
	if e.APIKey != "" {
		req.Header.Set("x-api-key", e.APIKey)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return errors.New(discovery.SafeMessage(err))
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("upstream returned HTTP %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, MaxResponseBytes+1))
	if err != nil {
		return errors.New(discovery.SafeMessage(err))
	}
	if len(data) > MaxResponseBytes {
		return errors.New("upstream response exceeds size limit")
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return errors.New("invalid upstream JSON response")
	}
	return nil
}

func (c *Client) Usage(ctx context.Context, e discovery.Endpoint) (*Usage, error) {
	var v *Usage
	if err := c.get(ctx, e, "usage", nil, &v); err != nil {
		return nil, err
	}
	if v == nil || strings.TrimSpace(v.Login) == "" || strings.TrimSpace(v.Login) != v.Login {
		return nil, errors.New("upstream usage has no valid login")
	}
	return v, nil
}

func validSummary(v *Summary, period string) bool {
	return v != nil && v.Period == period && v.Totals != nil && v.Models != nil && v.Totals.Costs != nil
}

func (c *Client) Summary(ctx context.Context, e discovery.Endpoint, period string) (*Summary, error) {
	var v *Summary
	if err := c.get(ctx, e, "token-usage", url.Values{"period": {period}}, &v); err != nil {
		return nil, err
	}
	if !validSummary(v, period) {
		return nil, errors.New("invalid upstream summary response")
	}
	return v, nil
}

func (c *Client) Daily(ctx context.Context, e discovery.Endpoint, period string) (*Daily, error) {
	var v *Daily
	if err := c.get(ctx, e, "token-usage/daily", url.Values{"period": {period}}, &v); err != nil {
		return nil, err
	}
	if v == nil || !validSummary(&v.Summary, period) || v.Days == nil {
		return nil, errors.New("invalid upstream daily response")
	}
	for _, day := range v.Days {
		if _, err := time.Parse("2006-01-02", day.Date); err != nil || day.Totals == nil || day.Models == nil {
			return nil, errors.New("invalid upstream daily bucket")
		}
	}
	return v, nil
}

func (c *Client) Events(ctx context.Context, e discovery.Endpoint, period string, page, size int) (*Events, error) {
	var v *Events
	q := url.Values{"period": {period}, "page": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(size)}}
	if err := c.get(ctx, e, "token-usage/events", q, &v); err != nil {
		return nil, err
	}
	if v == nil || v.Period != period || v.Items == nil || v.Page < 1 || v.PageSize < 1 || v.PageSize > 100 || v.Total < 0 || v.TotalPages < 0 {
		return nil, errors.New("invalid upstream events response")
	}
	return v, nil
}
