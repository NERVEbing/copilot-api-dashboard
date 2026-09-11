package sqlite

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/NERVEbing/copilot-api-dashboard/internal/upstream"
)

func testDay(date string, start time.Time, tokens, cost int64, model string) upstream.Day {
	totals := &upstream.Totals{
		Input: tokens - 1, Output: 1, Requests: 1, Tokens: tokens,
		Costs: []upstream.Cost{{Currency: "USD", Nanos: cost, Amount: 99}},
	}
	return upstream.Day{
		Date: date, StartMS: start.UnixMilli(), EndMS: start.Add(24 * time.Hour).UnixMilli(), Totals: totals,
		Models: []upstream.Model{{Model: model, Totals: *totals}},
	}
}

func testDaily(snapshotEnd int64, days ...upstream.Day) *upstream.Daily {
	snapshotDays := append([]upstream.Day(nil), days...)
	for i := range snapshotDays {
		if snapshotDays[i].EndMS > snapshotEnd {
			snapshotDays[i].EndMS = snapshotEnd
		}
	}
	return &upstream.Daily{Summary: upstream.Summary{Range: upstream.Range{EndMS: snapshotEnd}}, Days: snapshotDays}
}

func TestSaveDailyDetectsNonOverlappingRebuild(t *testing.T) {
	for _, gap := range []time.Duration{0, time.Minute} {
		t.Run(gap.String(), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "dashboard.sqlite")
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			start := time.Date(2026, 9, 11, 0, 0, 0, 0, time.Local)
			oldEnd := start.Add(9 * time.Hour)
			old := testDay("2026-09-11", start, 61, 522000, "claude-sonnet-5")
			if err := store.SaveDaily(ctx, "docker", "source-a", "http://example", "Alice", testDaily(oldEnd.UnixMilli(), old)); err != nil {
				t.Fatal(err)
			}
			// Reopen the existing database before recovering the interrupted history.
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			newStart := oldEnd.Add(gap)
			for i, tokens := range []int64{1000, 1000, 1200} {
				current := testDay("2026-09-11", newStart, tokens, tokens*1000, "gpt-5.6-sol")
				current.Totals.Requests = 10
				current.Totals.Output = 50
				current.Totals.Input = tokens - 50
				current.Models[0].Totals = *current.Totals
				end := oldEnd.Add(time.Duration(i+1) * time.Hour)
				if err := store.SaveDaily(ctx, "docker", "source-a", "http://example", "Alice", testDaily(end.UnixMilli(), current)); err != nil {
					t.Fatal(err)
				}
				var segments int
				if err := store.db.QueryRow("SELECT COUNT(*) FROM usage_segments").Scan(&segments); err != nil || segments != 2 {
					t.Fatalf("segments=%d err=%v", segments, err)
				}
				days, found, err := store.LoadDaily(ctx, "source-a", "http://example", "Alice", "lifetime", end)
				if err != nil || !found || len(days) != 1 {
					t.Fatalf("load: days=%+v found=%v err=%v", days, found, err)
				}
				day := days[0]
				if day.Totals.Tokens != 61+tokens || day.Totals.Requests != 11 ||
					len(day.Totals.Costs) != 1 || day.Totals.Costs[0].Nanos != 522000+tokens*1000 ||
					len(day.Models) != 2 || day.Models[0].Model != "claude-sonnet-5" || day.Models[0].Tokens != 61 ||
					day.Models[1].Model != "gpt-5.6-sol" || day.Models[1].Tokens != tokens {
					t.Fatalf("unexpected accumulated usage: %+v", day)
				}
			}
		})
	}
}

func TestSaveDailyKeepsReliableHistoryForOverlappingOrStaleSnapshots(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "overlapping", true: "stale"}[stale], func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(filepath.Join(t.TempDir(), "dashboard.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			start := time.Date(2026, 9, 11, 0, 0, 0, 0, time.Local)
			end := start.Add(10 * time.Hour)
			if err := store.SaveDaily(ctx, "docker", "source-a", "http://example", "Alice",
				testDaily(end.UnixMilli(), testDay("2026-09-11", start, 61, 10, "old-model"))); err != nil {
				t.Fatal(err)
			}
			before, _, err := store.LoadDaily(ctx, "source-a", "http://example", "Alice", "lifetime", end)
			if err != nil {
				t.Fatal(err)
			}
			currentEnd := end.Add(time.Hour)
			if stale {
				currentEnd = end.Add(-time.Minute)
			}
			current := testDay("2026-09-11", end.Add(-time.Hour), 1000, 100, "new-model")
			err = store.SaveDaily(ctx, "docker", "source-a", "http://example", "Alice", testDaily(currentEnd.UnixMilli(), current))
			if stale && err != nil || !stale && (err == nil || err.Error() != "daily usage snapshot is incomplete") {
				t.Fatalf("save error: %v", err)
			}
			after, _, err := store.LoadDaily(ctx, "source-a", "http://example", "Alice", "lifetime", end)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("reliable history changed: before=%+v after=%+v err=%v", before, after, err)
			}
			var segments int
			if err := store.db.QueryRow("SELECT COUNT(*) FROM usage_segments").Scan(&segments); err != nil || segments != 1 {
				t.Fatalf("segments=%d err=%v", segments, err)
			}
		})
	}
}

func TestSaveDailyAccumulatesResetSegmentsAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dashboard.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	day1Start := time.Date(2026, 9, 7, 0, 0, 0, 0, time.Local)
	day2Start := day1Start.AddDate(0, 0, 1)
	first := testDaily(day2Start.Add(12*time.Hour).UnixMilli(),
		testDay("2026-09-07", day1Start, 100, 10, "old-model"),
		testDay("2026-09-08", day2Start, 200, 20, "old-model"),
	)
	if err := store.SaveDaily(ctx, "docker", "account-a", "http://account-a:4141", "Alice", first); err != nil {
		t.Fatal(err)
	}
	updated := testDay("2026-09-08", day2Start, 250, 25, "old-model")
	newSnapshotEnd := day2Start.Add(13 * time.Hour).UnixMilli()
	if err := store.SaveDaily(ctx, "docker", "account-a", "http://account-a:4141", "Alice", testDaily(newSnapshotEnd, updated)); err != nil {
		t.Fatal(err)
	}
	reset := testDay("2026-09-08", day2Start, 50, 5, "new-model")
	resetSnapshotEnd := day2Start.Add(14 * time.Hour).UnixMilli()
	if err := store.SaveDaily(ctx, "docker", "account-a", "http://account-a:4141", "Alice", testDaily(resetSnapshotEnd, reset)); err != nil {
		t.Fatal(err)
	}
	grown := testDay("2026-09-08", day2Start, 55, 6, "new-model")
	latestSnapshotEnd := day2Start.Add(15 * time.Hour).UnixMilli()
	if err := store.SaveDaily(ctx, "docker", "account-a", "http://account-a:4141", "Alice", testDaily(latestSnapshotEnd, grown)); err != nil {
		t.Fatal(err)
	}
	stale := testDay("2026-09-08", day2Start, 25, 2, "stale-model")
	if err := store.SaveDaily(ctx, "docker", "account-a", "http://account-a:4141", "Alice", testDaily(latestSnapshotEnd-1, stale)); err != nil {
		t.Fatal(err)
	}
	closedCorrection := testDay("2026-09-07", day1Start, 1, 1, "incorrect-model")
	if err := store.SaveDaily(ctx, "docker", "account-a", "http://account-a:4141", "Alice", testDaily(latestSnapshotEnd+1, closedCorrection)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	days, found, err := store.LoadDaily(ctx, "account-a", "http://account-a:4141", "ALICE", "lifetime", day2Start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !found || len(days) != 2 || days[0].Totals.Tokens != 100 || days[1].Totals.Tokens != 305 || days[1].EndMS != latestSnapshotEnd {
		t.Fatalf("unexpected persisted days: %+v", days)
	}
	if len(days[1].Models) != 2 || days[1].Models[0].Model != "new-model" || days[1].Models[0].Tokens != 55 ||
		days[1].Models[1].Model != "old-model" || days[1].Models[1].Tokens != 250 ||
		len(days[1].Totals.Costs) != 1 || days[1].Totals.Costs[0].Nanos != 31 {
		t.Fatalf("stale model or cost details: %+v", days[1])
	}
	var segments int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM usage_segments`).Scan(&segments); err != nil || segments != 2 {
		t.Fatalf("usage segments: count=%d err=%v", segments, err)
	}
	var eventsTable int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='token_usage_events'`).Scan(&eventsTable); err != nil || eventsTable != 0 {
		t.Fatalf("request events table exists: count=%d err=%v", eventsTable, err)
	}
}

func TestSaveDailyFinalizesPreviousDayBeforeFreezing(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	day1Start := time.Date(2026, 9, 8, 0, 0, 0, 0, time.Local)
	day2Start := day1Start.AddDate(0, 0, 1)
	if err := store.SaveDaily(ctx, "docker", "account-a", "http://account-a:4141", "Alice",
		testDaily(day1Start.Add(12*time.Hour).UnixMilli(), testDay("2026-09-08", day1Start, 100, 10, "model-a"))); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDaily(ctx, "docker", "account-a", "http://account-a:4141", "Alice",
		testDaily(day2Start.Add(time.Hour).UnixMilli(),
			testDay("2026-09-08", day1Start, 120, 12, "model-a"),
			testDay("2026-09-09", day2Start, 10, 1, "model-b"))); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDaily(ctx, "docker", "account-a", "http://account-a:4141", "Alice",
		testDaily(day2Start.Add(2*time.Hour).UnixMilli(),
			testDay("2026-09-08", day1Start, 1, 1, "incorrect-model"),
			testDay("2026-09-09", day2Start, 20, 2, "model-b"))); err != nil {
		t.Fatal(err)
	}
	days, found, err := store.LoadDaily(ctx, "account-a", "http://account-a:4141", "Alice", "lifetime", day2Start.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !found || len(days) != 2 || days[0].Totals.Tokens != 120 || days[0].EndMS != day2Start.UnixMilli() ||
		days[1].Totals.Tokens != 20 {
		t.Fatalf("unexpected finalized days: %+v", days)
	}
	if len(days[0].Totals.Costs) != 1 || days[0].Totals.Costs[0].Nanos != 12 ||
		len(days[0].Models) != 1 || days[0].Models[0].Model != "model-a" || days[0].Models[0].Tokens != 120 {
		t.Fatalf("finalized day was overwritten: %+v", days[0])
	}
	var segments int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM usage_segments`).Scan(&segments); err != nil || segments != 1 {
		t.Fatalf("usage segments: count=%d err=%v", segments, err)
	}
}

func TestSaveDailyRejectsIncompleteCurrentDetails(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.Local)
	current := testDay("2026-09-09", time.Date(2026, 9, 9, 0, 0, 0, 0, time.Local), 100, 10, "model-a")
	if err := store.SaveDaily(ctx, "yaml", "source-a", "http://example", "Alice", testDaily(now.UnixMilli(), current)); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 9, 0, 0, 0, 0, time.Local)
	cases := []struct {
		name   string
		change func(upstream.Day) upstream.Day
	}{
		{"missing costs", func(day upstream.Day) upstream.Day {
			day.Totals.Costs = nil
			day.Models[0].Costs = nil
			return day
		}},
		{"decreasing costs", func(day upstream.Day) upstream.Day {
			day.Totals.Costs[0].Nanos = 9
			day.Models[0].Costs[0].Nanos = 9
			return day
		}},
		{"missing models", func(day upstream.Day) upstream.Day {
			day.Models = nil
			return day
		}},
	}
	for i, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			incomplete := test.change(testDay("2026-09-09", start, 110, 11, "model-a"))
			err := store.SaveDaily(ctx, "yaml", "source-a", "http://example", "Alice", testDaily(now.Add(time.Duration(i+1)*time.Hour).UnixMilli(), incomplete))
			if err == nil || err.Error() != "daily usage snapshot is incomplete" {
				t.Fatalf("incomplete snapshot error: %v", err)
			}
		})
	}
	days, found, err := store.LoadDaily(ctx, "source-a", "http://example", "Alice", "lifetime", now)
	if err != nil || !found || len(days) != 1 || days[0].Totals.Tokens != 100 ||
		len(days[0].Totals.Costs) != 1 || days[0].Totals.Costs[0].Nanos != 10 || len(days[0].Models) != 1 {
		t.Fatalf("reliable snapshot changed: found=%v days=%+v err=%v", found, days, err)
	}
}

func TestLoadDailyFiltersPeriodAndSource(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.Local)
	old := testDay("2026-08-01", time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local), 10, 1, "model-a")
	today := testDay("2026-09-09", time.Date(2026, 9, 9, 0, 0, 0, 0, time.Local), 20, 2, "model-a")
	for _, name := range []string{"source-a", "source-b"} {
		if err := store.SaveDaily(ctx, "yaml", name, "http://example", "Alice", testDaily(now.UnixMilli(), old, today)); err != nil {
			t.Fatal(err)
		}
	}
	days, found, err := store.LoadDaily(ctx, "source-a", "http://example", "Alice", "today", now)
	if err != nil || !found || len(days) != 1 || days[0].Totals.Tokens != 20 {
		t.Fatalf("period filter: found=%v days=%+v err=%v", found, days, err)
	}
	missing, found, err := store.LoadDaily(ctx, "missing", "http://missing", "Alice", "lifetime", now)
	if err != nil || found || missing != nil {
		t.Fatalf("source isolation: found=%v days=%+v err=%v", found, missing, err)
	}
}

func TestLoadDailyDistinguishesMissingAndEmptySnapshots(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.Local)
	days, found, err := store.LoadDaily(ctx, "source-a", "http://example", "Alice", "lifetime", now)
	if err != nil || found || days != nil {
		t.Fatalf("missing snapshot: found=%v days=%+v err=%v", found, days, err)
	}
	if err := store.SaveDaily(ctx, "yaml", "source-a", "http://example", "Alice", testDaily(now.UnixMilli())); err != nil {
		t.Fatal(err)
	}
	days, found, err = store.LoadDaily(ctx, "source-a", "http://example", "Alice", "lifetime", now)
	if err != nil || !found || days == nil || len(days) != 0 {
		t.Fatalf("empty snapshot: found=%v days=%+v err=%v", found, days, err)
	}
}

func TestSourceIdentitySurvivesDiscoveryChanges(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.Local)
	tests := []struct {
		name              string
		yamlName, yamlURL string
	}{
		{name: "matching URL", yamlName: "configured-account", yamlURL: "http://account-a:4141"},
		{name: "matching name", yamlName: "account-a", yamlURL: "https://proxy.example/copilot"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "dashboard.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			}()
			day := testDay("2026-09-09", time.Date(2026, 9, 9, 0, 0, 0, 0, time.Local), 100, 10, "model-a")
			if err := store.SaveDaily(ctx, "docker", "account-a", "http://account-a:4141", "Alice", testDaily(now.UnixMilli(), day)); err != nil {
				t.Fatal(err)
			}
			days, found, err := store.LoadDaily(ctx, test.yamlName, test.yamlURL, "Alice", "lifetime", now)
			if err != nil || !found || len(days) != 1 || days[0].Totals.Tokens != 100 {
				t.Fatalf("history not preserved: found=%v days=%+v err=%v", found, days, err)
			}
			day.Totals.Tokens = 110
			day.Totals.Input = 109
			if err := store.SaveDaily(ctx, "yaml", test.yamlName, test.yamlURL, "Alice", testDaily(now.Add(time.Hour).UnixMilli(), day)); err != nil {
				t.Fatal(err)
			}
			var sources, accounts int
			if err := store.db.QueryRow("SELECT COUNT(*) FROM sources").Scan(&sources); err != nil {
				t.Fatal(err)
			}
			if err := store.db.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&accounts); err != nil {
				t.Fatal(err)
			}
			if sources != 1 || accounts != 1 {
				t.Fatalf("history split: sources=%d accounts=%d", sources, accounts)
			}
			var sourceType, name, normalizedURL string
			if err := store.db.QueryRow("SELECT source_type, name, normalized_url FROM sources").Scan(&sourceType, &name, &normalizedURL); err != nil {
				t.Fatal(err)
			}
			if sourceType != "yaml" || name != test.yamlName || normalizedURL != test.yamlURL {
				t.Fatalf("source metadata not updated: type=%s name=%s url=%s", sourceType, name, normalizedURL)
			}
			days, found, err = store.LoadDaily(ctx, test.yamlName, test.yamlURL, "Alice", "lifetime", now.Add(2*time.Hour))
			if err != nil || !found || len(days) != 1 || days[0].Totals.Tokens != 110 {
				t.Fatalf("updated history unavailable: found=%v days=%+v err=%v", found, days, err)
			}
		})
	}
}

func TestSourceIdentityRejectsAmbiguousMatches(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.Local)
	day := testDay("2026-09-09", time.Date(2026, 9, 9, 0, 0, 0, 0, time.Local), 100, 10, "model-a")
	if err := store.SaveDaily(ctx, "docker", "account-a", "http://account-a:4141", "Alice", testDaily(now.UnixMilli(), day)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDaily(ctx, "yaml", "account-b", "http://account-b:4141", "Bob", testDaily(now.UnixMilli(), day)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDaily(ctx, "yaml", "account-a", "http://account-b:4141", "Alice", testDaily(now.Add(time.Hour).UnixMilli(), day)); err == nil || err.Error() != "persisted source identity is ambiguous" {
		t.Fatalf("ambiguous source error: %v", err)
	}
	var sources, accounts int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM sources").Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if sources != 2 || accounts != 2 {
		t.Fatalf("ambiguous source changed database: sources=%d accounts=%d", sources, accounts)
	}
}
