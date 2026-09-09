package sqlite

import (
	"context"
	"path/filepath"
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
	return &upstream.Daily{Summary: upstream.Summary{Range: upstream.Range{EndMS: snapshotEnd}}, Days: days}
}

func TestSaveDailyReplacesStableDayAndSurvivesRestart(t *testing.T) {
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
	updated := testDay("2026-09-08", day2Start, 50, 5, "new-model")
	updated.EndMS += int64(time.Hour / time.Millisecond)
	newSnapshotEnd := day2Start.Add(13 * time.Hour).UnixMilli()
	if err := store.SaveDaily(ctx, "docker", "account-a", "http://account-a:4141", "Alice", testDaily(newSnapshotEnd, updated)); err != nil {
		t.Fatal(err)
	}
	corrected := testDay("2026-09-08", day2Start, 55, 6, "corrected-model")
	corrected.EndMS = updated.EndMS
	if err := store.SaveDaily(ctx, "docker", "account-a", "http://account-a:4141", "Alice", testDaily(newSnapshotEnd, corrected)); err != nil {
		t.Fatal(err)
	}
	stale := testDay("2026-09-08", day2Start, 25, 2, "stale-model")
	if err := store.SaveDaily(ctx, "docker", "account-a", "http://account-a:4141", "Alice", testDaily(newSnapshotEnd-1, stale)); err != nil {
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
	days, found, err := store.LoadDaily(ctx, "docker", "account-a", "ALICE", "lifetime", day2Start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !found || len(days) != 2 || days[0].Totals.Tokens != 100 || days[1].Totals.Tokens != 55 || days[1].EndMS != updated.EndMS {
		t.Fatalf("unexpected persisted days: %+v", days)
	}
	if len(days[1].Models) != 1 || days[1].Models[0].Model != "corrected-model" || len(days[1].Totals.Costs) != 1 || days[1].Totals.Costs[0].Nanos != 6 {
		t.Fatalf("stale model or cost details: %+v", days[1])
	}
	var eventsTable int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='token_usage_events'`).Scan(&eventsTable); err != nil || eventsTable != 0 {
		t.Fatalf("request events table exists: count=%d err=%v", eventsTable, err)
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
	days, found, err := store.LoadDaily(ctx, "yaml", "source-a", "Alice", "today", now)
	if err != nil || !found || len(days) != 1 || days[0].Totals.Tokens != 20 {
		t.Fatalf("period filter: found=%v days=%+v err=%v", found, days, err)
	}
	missing, found, err := store.LoadDaily(ctx, "yaml", "missing", "Alice", "lifetime", now)
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
	days, found, err := store.LoadDaily(ctx, "yaml", "source-a", "Alice", "lifetime", now)
	if err != nil || found || days != nil {
		t.Fatalf("missing snapshot: found=%v days=%+v err=%v", found, days, err)
	}
	if err := store.SaveDaily(ctx, "yaml", "source-a", "http://example", "Alice", testDaily(now.UnixMilli())); err != nil {
		t.Fatal(err)
	}
	days, found, err = store.LoadDaily(ctx, "yaml", "source-a", "Alice", "lifetime", now)
	if err != nil || !found || days == nil || len(days) != 0 {
		t.Fatalf("empty snapshot: found=%v days=%+v err=%v", found, days, err)
	}
}
