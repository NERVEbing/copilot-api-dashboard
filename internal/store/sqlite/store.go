package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/NERVEbing/copilot-api-dashboard/internal/upstream"
)

const schemaVersion = 2

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is empty")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, errors.New("cannot create database directory")
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, errors.New("cannot open database")
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) initialize(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return errors.New("cannot read database schema version")
	}
	if version != 0 && version != schemaVersion {
		return fmt.Errorf("unsupported database schema version %d", version)
	}
	statements := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		`CREATE TABLE IF NOT EXISTS sources (
			id INTEGER PRIMARY KEY,
			source_key TEXT NOT NULL UNIQUE,
			source_type TEXT NOT NULL,
			name TEXT NOT NULL,
			normalized_url TEXT NOT NULL,
			last_seen_ms INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS accounts (
			id INTEGER PRIMARY KEY,
			source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
			login_key TEXT NOT NULL,
			login TEXT NOT NULL,
			first_seen_ms INTEGER NOT NULL,
			last_seen_ms INTEGER NOT NULL,
			UNIQUE(source_id, login_key)
		)`,
		`CREATE TABLE IF NOT EXISTS usage_segments (
			id INTEGER PRIMARY KEY,
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			ordinal INTEGER NOT NULL,
			start_date TEXT NOT NULL,
			first_seen_ms INTEGER NOT NULL,
			last_seen_ms INTEGER NOT NULL,
			UNIQUE(account_id, ordinal)
		)`,
		`CREATE TABLE IF NOT EXISTS daily_usage (
			segment_id INTEGER NOT NULL REFERENCES usage_segments(id) ON DELETE CASCADE,
			date TEXT NOT NULL,
			start_ms INTEGER NOT NULL,
			end_ms INTEGER NOT NULL,
			snapshot_end_ms INTEGER NOT NULL,
			input_tokens INTEGER NOT NULL,
			output_tokens INTEGER NOT NULL,
			cache_read_input_tokens INTEGER NOT NULL,
			cache_creation_input_tokens INTEGER NOT NULL,
			request_count INTEGER NOT NULL,
			total_tokens INTEGER NOT NULL,
			total_nano_aiu INTEGER,
			PRIMARY KEY(segment_id, date)
		)`,
		`CREATE TABLE IF NOT EXISTS daily_usage_costs (
			segment_id INTEGER NOT NULL,
			date TEXT NOT NULL,
			currency TEXT NOT NULL,
			total_cost_nanos INTEGER NOT NULL,
			PRIMARY KEY(segment_id, date, currency),
			FOREIGN KEY(segment_id, date) REFERENCES daily_usage(segment_id, date) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS daily_model_usage (
			segment_id INTEGER NOT NULL,
			date TEXT NOT NULL,
			model TEXT NOT NULL,
			input_tokens INTEGER NOT NULL,
			output_tokens INTEGER NOT NULL,
			cache_read_input_tokens INTEGER NOT NULL,
			cache_creation_input_tokens INTEGER NOT NULL,
			request_count INTEGER NOT NULL,
			total_tokens INTEGER NOT NULL,
			total_nano_aiu INTEGER,
			PRIMARY KEY(segment_id, date, model),
			FOREIGN KEY(segment_id, date) REFERENCES daily_usage(segment_id, date) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS daily_model_costs (
			segment_id INTEGER NOT NULL,
			date TEXT NOT NULL,
			model TEXT NOT NULL,
			currency TEXT NOT NULL,
			total_cost_nanos INTEGER NOT NULL,
			PRIMARY KEY(segment_id, date, model, currency),
			FOREIGN KEY(segment_id, date, model) REFERENCES daily_model_usage(segment_id, date, model) ON DELETE CASCADE
		)`,
		fmt.Sprintf("PRAGMA user_version = %d", schemaVersion),
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return errors.New("cannot initialize database")
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func sourceKey(sourceType, name string) string { return sourceType + "\x00" + name }

type usageSegment struct {
	id        int64
	ordinal   int
	startDate string
}

type persistedDay struct {
	endMS       int64
	snapshotEnd int64
	totals      upstream.Totals
	models      map[string]upstream.Totals
}

func (s *Store) SaveDaily(ctx context.Context, sourceType, name, normalizedURL, login string, daily *upstream.Daily) error {
	if err := validateDaily(daily); err != nil {
		return err
	}
	days := append([]upstream.Day(nil), daily.Days...)
	sort.Slice(days, func(i, j int) bool {
		if days[i].StartMS != days[j].StartMS {
			return days[i].StartMS < days[j].StartMS
		}
		return days[i].Date < days[j].Date
	})

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("cannot begin persistence transaction")
	}
	defer func() { _ = tx.Rollback() }()

	accountID, now, err := persistAccount(ctx, tx, sourceType, name, normalizedURL, login)
	if err != nil {
		return err
	}
	if len(days) == 0 {
		if err := tx.Commit(); err != nil {
			return errors.New("cannot commit persisted usage")
		}
		return nil
	}

	segment, err := latestSegment(ctx, tx, accountID)
	if err != nil {
		return err
	}
	if segment == nil {
		segment, err = createSegment(ctx, tx, accountID, 0, days[0].Date, now)
		if err != nil {
			return err
		}
	}

	mutableDays, err := loadMutableDays(ctx, tx, segment.id)
	if err != nil {
		return err
	}
	resetDate := findResetDate(segment, daily.Range.EndMS, days, mutableDays)
	if resetDate == "" {
		if err := validateDetails(ctx, tx, segment, daily.Range.EndMS, days, mutableDays); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE usage_segments SET last_seen_ms = ? WHERE id = ?", now, segment.id); err != nil {
			return errors.New("cannot update usage segment")
		}
	} else {
		segment, err = createSegment(ctx, tx, accountID, segment.ordinal+1, resetDate, now)
		if err != nil {
			return err
		}
	}

	for _, day := range days {
		if day.Date < segment.startDate {
			continue
		}
		if err := replaceDay(ctx, tx, segment.id, daily.Range.EndMS, day); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return errors.New("cannot commit persisted usage")
	}
	return nil
}

func validateDaily(daily *upstream.Daily) error {
	if daily == nil || daily.Range.EndMS <= 0 {
		return errors.New("invalid daily usage snapshot")
	}
	seen := map[string]bool{}
	for _, day := range daily.Days {
		if day.Date == "" || day.Totals == nil || day.StartMS < 0 || day.EndMS <= day.StartMS ||
			day.EndMS > daily.Range.EndMS || seen[day.Date] {
			return errors.New("invalid daily usage snapshot")
		}
		seen[day.Date] = true
	}
	return nil
}

func persistAccount(ctx context.Context, tx *sql.Tx, sourceType, name, normalizedURL, login string) (int64, int64, error) {
	now := time.Now().UnixMilli()
	key := sourceKey(sourceType, name)
	if _, err := tx.ExecContext(ctx, `INSERT INTO sources(source_key, source_type, name, normalized_url, last_seen_ms)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(source_key) DO UPDATE SET source_type=excluded.source_type, name=excluded.name,
		normalized_url=excluded.normalized_url, last_seen_ms=excluded.last_seen_ms`, key, sourceType, name, normalizedURL, now); err != nil {
		return 0, 0, errors.New("cannot persist source")
	}
	var sourceID int64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM sources WHERE source_key = ?", key).Scan(&sourceID); err != nil {
		return 0, 0, errors.New("cannot resolve persisted source")
	}
	loginKey := strings.ToLower(login)
	if _, err := tx.ExecContext(ctx, `INSERT INTO accounts(source_id, login_key, login, first_seen_ms, last_seen_ms)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(source_id, login_key) DO UPDATE SET login=excluded.login, last_seen_ms=excluded.last_seen_ms`, sourceID, loginKey, login, now, now); err != nil {
		return 0, 0, errors.New("cannot persist account")
	}
	var accountID int64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM accounts WHERE source_id = ? AND login_key = ?", sourceID, loginKey).Scan(&accountID); err != nil {
		return 0, 0, errors.New("cannot resolve persisted account")
	}
	return accountID, now, nil
}

func latestSegment(ctx context.Context, tx *sql.Tx, accountID int64) (*usageSegment, error) {
	var segment usageSegment
	err := tx.QueryRowContext(ctx, `SELECT id, ordinal, start_date FROM usage_segments
		WHERE account_id = ? ORDER BY ordinal DESC LIMIT 1`, accountID).Scan(&segment.id, &segment.ordinal, &segment.startDate)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("cannot resolve usage segment")
	}
	return &segment, nil
}

func createSegment(ctx context.Context, tx *sql.Tx, accountID int64, ordinal int, startDate string, now int64) (*usageSegment, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO usage_segments(account_id, ordinal, start_date, first_seen_ms, last_seen_ms)
		VALUES(?, ?, ?, ?, ?)`, accountID, ordinal, startDate, now, now)
	if err != nil {
		return nil, errors.New("cannot create usage segment")
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, errors.New("cannot resolve usage segment")
	}
	return &usageSegment{id: id, ordinal: ordinal, startDate: startDate}, nil
}

func findResetDate(segment *usageSegment, snapshotEnd int64, days []upstream.Day, storedDays map[string]*persistedDay) string {
	resetDate := ""
	for _, day := range days {
		if day.Date < segment.startDate {
			continue
		}
		stored := storedDays[day.Date]
		if stored == nil || snapshotEnd < stored.snapshotEnd {
			continue
		}
		if totalsRegressed(stored.totals, *day.Totals) && (resetDate == "" || day.Date < resetDate) {
			resetDate = day.Date
		}
	}
	return resetDate
}

func validateDetails(ctx context.Context, tx *sql.Tx, segment *usageSegment, snapshotEnd int64, days []upstream.Day, storedDays map[string]*persistedDay) error {
	for _, day := range days {
		if day.Date < segment.startDate {
			continue
		}
		stored := storedDays[day.Date]
		if stored == nil || snapshotEnd < stored.snapshotEnd {
			continue
		}
		if err := loadPersistedDetails(ctx, tx, segment.id, day.Date, stored); err != nil {
			return err
		}
		if detailsRegressed(stored, day) {
			return errors.New("daily usage snapshot is incomplete")
		}
	}
	return nil
}

func loadMutableDays(ctx context.Context, tx *sql.Tx, segmentID int64) (map[string]*persistedDay, error) {
	rows, err := tx.QueryContext(ctx, `SELECT date, end_ms, snapshot_end_ms, input_tokens, output_tokens,
		cache_read_input_tokens, cache_creation_input_tokens, request_count, total_tokens, total_nano_aiu
		FROM daily_usage WHERE segment_id = ? AND end_ms = snapshot_end_ms`, segmentID)
	if err != nil {
		return nil, errors.New("cannot read mutable persisted usage")
	}
	days := map[string]*persistedDay{}
	for rows.Next() {
		var date string
		var nano sql.NullInt64
		stored := &persistedDay{models: map[string]upstream.Totals{}}
		if err := rows.Scan(&date, &stored.endMS, &stored.snapshotEnd, &stored.totals.Input, &stored.totals.Output,
			&stored.totals.CacheRead, &stored.totals.CacheCreation, &stored.totals.Requests,
			&stored.totals.Tokens, &nano); err != nil {
			_ = rows.Close()
			return nil, errors.New("cannot decode mutable persisted usage")
		}
		if nano.Valid {
			stored.totals.NanoAIU = new(int64)
			*stored.totals.NanoAIU = nano.Int64
		}
		days[date] = stored
	}
	if err := rows.Close(); err != nil || rows.Err() != nil {
		return nil, errors.New("cannot read mutable persisted usage")
	}
	return days, nil
}

func loadPersistedDetails(ctx context.Context, tx *sql.Tx, segmentID int64, date string, stored *persistedDay) error {
	costRows, err := tx.QueryContext(ctx, `SELECT currency, total_cost_nanos FROM daily_usage_costs
		WHERE segment_id = ? AND date = ?`, segmentID, date)
	if err != nil {
		return errors.New("cannot read persisted costs")
	}
	for costRows.Next() {
		var cost upstream.Cost
		if err := costRows.Scan(&cost.Currency, &cost.Nanos); err != nil {
			_ = costRows.Close()
			return errors.New("cannot decode persisted costs")
		}
		stored.totals.Costs = append(stored.totals.Costs, cost)
	}
	if err := costRows.Close(); err != nil || costRows.Err() != nil {
		return errors.New("cannot read persisted costs")
	}
	modelRows, err := tx.QueryContext(ctx, `SELECT model, input_tokens, output_tokens, cache_read_input_tokens,
		cache_creation_input_tokens, request_count, total_tokens, total_nano_aiu
		FROM daily_model_usage WHERE segment_id = ? AND date = ?`, segmentID, date)
	if err != nil {
		return errors.New("cannot read persisted model usage")
	}
	for modelRows.Next() {
		var model string
		var totals upstream.Totals
		var modelNano sql.NullInt64
		if err := modelRows.Scan(&model, &totals.Input, &totals.Output, &totals.CacheRead,
			&totals.CacheCreation, &totals.Requests, &totals.Tokens, &modelNano); err != nil {
			_ = modelRows.Close()
			return errors.New("cannot decode persisted model usage")
		}
		if modelNano.Valid {
			totals.NanoAIU = new(int64)
			*totals.NanoAIU = modelNano.Int64
		}
		stored.models[model] = totals
	}
	if err := modelRows.Close(); err != nil || modelRows.Err() != nil {
		return errors.New("cannot read persisted model usage")
	}
	modelCostRows, err := tx.QueryContext(ctx, `SELECT model, currency, total_cost_nanos FROM daily_model_costs
		WHERE segment_id = ? AND date = ?`, segmentID, date)
	if err != nil {
		return errors.New("cannot read persisted model costs")
	}
	for modelCostRows.Next() {
		var model string
		var cost upstream.Cost
		if err := modelCostRows.Scan(&model, &cost.Currency, &cost.Nanos); err != nil {
			_ = modelCostRows.Close()
			return errors.New("cannot decode persisted model costs")
		}
		totals := stored.models[model]
		totals.Costs = append(totals.Costs, cost)
		stored.models[model] = totals
	}
	if err := modelCostRows.Close(); err != nil || modelCostRows.Err() != nil {
		return errors.New("cannot read persisted model costs")
	}
	return nil
}

func totalsRegressed(old, current upstream.Totals) bool {
	if current.Input < old.Input || current.Output < old.Output || current.CacheRead < old.CacheRead ||
		current.CacheCreation < old.CacheCreation || current.Requests < old.Requests || current.Tokens < old.Tokens {
		return true
	}
	return old.NanoAIU != nil && current.NanoAIU != nil && *current.NanoAIU < *old.NanoAIU
}

func costsRegressed(old, current []upstream.Cost) bool {
	values := make(map[string]int64, len(current))
	for _, cost := range current {
		values[cost.Currency] = cost.Nanos
	}
	for _, cost := range old {
		value, ok := values[cost.Currency]
		if !ok || value < cost.Nanos {
			return true
		}
	}
	return false
}

func detailsRegressed(stored *persistedDay, current upstream.Day) bool {
	if stored.totals.NanoAIU != nil && current.Totals.NanoAIU == nil {
		return true
	}
	if costsRegressed(stored.totals.Costs, current.Totals.Costs) {
		return true
	}
	models := make(map[string]upstream.Totals, len(current.Models))
	for _, model := range current.Models {
		models[model.Model] = model.Totals
	}
	for name, old := range stored.models {
		value, ok := models[name]
		if !ok || totalsRegressed(old, value) || old.NanoAIU != nil && value.NanoAIU == nil || costsRegressed(old.Costs, value.Costs) {
			return true
		}
	}
	return false
}

func replaceDay(ctx context.Context, tx *sql.Tx, segmentID, snapshotEnd int64, day upstream.Day) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO daily_usage(
		segment_id, date, start_ms, end_ms, snapshot_end_ms, input_tokens, output_tokens,
		cache_read_input_tokens, cache_creation_input_tokens, request_count,
		total_tokens, total_nano_aiu
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(segment_id, date) DO UPDATE SET
		start_ms=excluded.start_ms, end_ms=excluded.end_ms, snapshot_end_ms=excluded.snapshot_end_ms,
		input_tokens=excluded.input_tokens, output_tokens=excluded.output_tokens,
		cache_read_input_tokens=excluded.cache_read_input_tokens,
		cache_creation_input_tokens=excluded.cache_creation_input_tokens,
		request_count=excluded.request_count, total_tokens=excluded.total_tokens,
		total_nano_aiu=excluded.total_nano_aiu
	WHERE excluded.snapshot_end_ms >= daily_usage.snapshot_end_ms
		AND daily_usage.end_ms = daily_usage.snapshot_end_ms`,
		segmentID, day.Date, day.StartMS, day.EndMS, snapshotEnd, day.Totals.Input, day.Totals.Output,
		day.Totals.CacheRead, day.Totals.CacheCreation, day.Totals.Requests,
		day.Totals.Tokens, nullableInt64(day.Totals.NanoAIU))
	if err != nil {
		return errors.New("cannot persist daily usage")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return errors.New("cannot determine persisted usage result")
	}
	if changed == 0 {
		return nil
	}
	for _, table := range []string{"daily_usage_costs", "daily_model_costs", "daily_model_usage"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE segment_id = ? AND date = ?", segmentID, day.Date); err != nil {
			return errors.New("cannot replace persisted daily details")
		}
	}
	for _, cost := range day.Totals.Costs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO daily_usage_costs(segment_id, date, currency, total_cost_nanos) VALUES(?, ?, ?, ?)`, segmentID, day.Date, cost.Currency, cost.Nanos); err != nil {
			return errors.New("cannot persist daily cost")
		}
	}
	for _, model := range day.Models {
		if _, err := tx.ExecContext(ctx, `INSERT INTO daily_model_usage(
			segment_id, date, model, input_tokens, output_tokens, cache_read_input_tokens,
			cache_creation_input_tokens, request_count, total_tokens, total_nano_aiu
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, segmentID, day.Date, model.Model,
			model.Input, model.Output, model.CacheRead, model.CacheCreation, model.Requests,
			model.Tokens, nullableInt64(model.NanoAIU)); err != nil {
			return errors.New("cannot persist daily model usage")
		}
		for _, cost := range model.Costs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO daily_model_costs(segment_id, date, model, currency, total_cost_nanos) VALUES(?, ?, ?, ?, ?)`, segmentID, day.Date, model.Model, cost.Currency, cost.Nanos); err != nil {
				return errors.New("cannot persist daily model cost")
			}
		}
	}
	return nil
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func periodStart(period string, now time.Time) int64 {
	if period == "lifetime" {
		return 0
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch period {
	case "this_week":
		start = start.AddDate(0, 0, -int((start.Weekday()+6)%7))
	case "last_7_days":
		start = start.AddDate(0, 0, -6)
	case "this_month":
		start = time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, start.Location())
	case "last_30_days":
		start = start.AddDate(0, 0, -29)
	}
	return start.UnixMilli()
}

func (s *Store) LoadDaily(ctx context.Context, sourceType, name, login, period string, now time.Time) ([]upstream.Day, bool, error) {
	start, end := periodStart(period, now), now.UnixMilli()+1
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, errors.New("cannot begin persistence read transaction")
	}
	defer func() { _ = tx.Rollback() }()
	var accountID int64
	err = tx.QueryRowContext(ctx, `SELECT a.id FROM accounts a
		JOIN sources s ON s.id = a.source_id
		WHERE s.source_key = ? AND a.login_key = ?`, sourceKey(sourceType, name), strings.ToLower(login)).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return nil, false, errors.New("cannot commit persistence read transaction")
		}
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errors.New("cannot resolve persisted account")
	}
	rows, err := tx.QueryContext(ctx, `SELECT d.date, d.start_ms, d.end_ms, d.input_tokens,
		d.output_tokens, d.cache_read_input_tokens, d.cache_creation_input_tokens,
		d.request_count, d.total_tokens, d.total_nano_aiu
	FROM daily_usage d
	JOIN usage_segments g ON g.id = d.segment_id
	WHERE g.account_id = ? AND d.start_ms >= ? AND d.start_ms < ?
	ORDER BY d.date, g.ordinal`, accountID, start, end)
	if err != nil {
		return nil, false, errors.New("cannot read persisted usage")
	}
	days := []upstream.Day{}
	byDate := map[string]int{}
	for rows.Next() {
		var segmentDay upstream.Day
		var nano sql.NullInt64
		segmentDay.Totals = &upstream.Totals{Costs: []upstream.Cost{}}
		if err := rows.Scan(&segmentDay.Date, &segmentDay.StartMS, &segmentDay.EndMS, &segmentDay.Totals.Input, &segmentDay.Totals.Output,
			&segmentDay.Totals.CacheRead, &segmentDay.Totals.CacheCreation, &segmentDay.Totals.Requests,
			&segmentDay.Totals.Tokens, &nano); err != nil {
			_ = rows.Close()
			return nil, false, errors.New("cannot decode persisted usage")
		}
		if nano.Valid {
			segmentDay.Totals.NanoAIU = new(int64)
			*segmentDay.Totals.NanoAIU = nano.Int64
		}
		index, ok := byDate[segmentDay.Date]
		if !ok {
			days = append(days, upstream.Day{
				Date: segmentDay.Date, StartMS: segmentDay.StartMS, EndMS: segmentDay.EndMS,
				Totals: &upstream.Totals{Costs: []upstream.Cost{}}, Models: []upstream.Model{},
			})
			index = len(days) - 1
			byDate[segmentDay.Date] = index
		} else {
			if segmentDay.StartMS < days[index].StartMS {
				days[index].StartMS = segmentDay.StartMS
			}
			if segmentDay.EndMS > days[index].EndMS {
				days[index].EndMS = segmentDay.EndMS
			}
		}
		addStoredTotals(days[index].Totals, *segmentDay.Totals)
	}
	if err := rows.Close(); err != nil || rows.Err() != nil {
		return nil, false, errors.New("cannot read persisted usage")
	}
	if len(days) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, false, errors.New("cannot commit persistence read transaction")
		}
		return days, true, nil
	}
	if err := s.loadCosts(ctx, tx, accountID, start, end, days, byDate); err != nil {
		return nil, false, err
	}
	if err := s.loadModels(ctx, tx, accountID, start, end, days, byDate); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, errors.New("cannot commit persistence read transaction")
	}
	return days, true, nil
}

func addStoredCost(costs *[]upstream.Cost, currency string, nanos int64) {
	for i := range *costs {
		if (*costs)[i].Currency == currency {
			(*costs)[i].Nanos += nanos
			(*costs)[i].Amount = float64((*costs)[i].Nanos) / 1e9
			return
		}
	}
	*costs = append(*costs, upstream.Cost{Currency: currency, Nanos: nanos, Amount: float64(nanos) / 1e9})
}

func addStoredTotals(dst *upstream.Totals, src upstream.Totals) {
	dst.Input += src.Input
	dst.Output += src.Output
	dst.CacheRead += src.CacheRead
	dst.CacheCreation += src.CacheCreation
	dst.Requests += src.Requests
	dst.Tokens += src.Tokens
	if src.NanoAIU != nil {
		if dst.NanoAIU == nil {
			dst.NanoAIU = new(int64)
		}
		*dst.NanoAIU += *src.NanoAIU
	}
}

func (s *Store) loadCosts(ctx context.Context, tx *sql.Tx, accountID, start, end int64, days []upstream.Day, byDate map[string]int) error {
	rows, err := tx.QueryContext(ctx, `SELECT c.date, c.currency, c.total_cost_nanos
	FROM daily_usage_costs c
	JOIN daily_usage d ON d.segment_id = c.segment_id AND d.date = c.date
	JOIN usage_segments g ON g.id = c.segment_id
	WHERE g.account_id = ? AND d.start_ms >= ? AND d.start_ms < ?
	ORDER BY c.date, c.currency`, accountID, start, end)
	if err != nil {
		return errors.New("cannot read persisted costs")
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var date, currency string
		var nanos int64
		if err := rows.Scan(&date, &currency, &nanos); err != nil {
			return errors.New("cannot decode persisted costs")
		}
		if index, ok := byDate[date]; ok {
			addStoredCost(&days[index].Totals.Costs, currency, nanos)
		}
	}
	if rows.Err() != nil {
		return errors.New("cannot read persisted costs")
	}
	for i := range days {
		sort.Slice(days[i].Totals.Costs, func(a, b int) bool { return days[i].Totals.Costs[a].Currency < days[i].Totals.Costs[b].Currency })
	}
	return nil
}

func (s *Store) loadModels(ctx context.Context, tx *sql.Tx, accountID, start, end int64, days []upstream.Day, byDate map[string]int) error {
	rows, err := tx.QueryContext(ctx, `SELECT m.date, m.model, m.input_tokens, m.output_tokens,
		m.cache_read_input_tokens, m.cache_creation_input_tokens, m.request_count,
		m.total_tokens, m.total_nano_aiu
	FROM daily_model_usage m
	JOIN daily_usage d ON d.segment_id = m.segment_id AND d.date = m.date
	JOIN usage_segments g ON g.id = m.segment_id
	WHERE g.account_id = ? AND d.start_ms >= ? AND d.start_ms < ?
	ORDER BY m.date, m.model`, accountID, start, end)
	if err != nil {
		return errors.New("cannot read persisted model usage")
	}
	models := map[struct{ date, model string }]struct{ day, model int }{}
	for rows.Next() {
		var date string
		var model upstream.Model
		var nano sql.NullInt64
		model.Costs = []upstream.Cost{}
		if err := rows.Scan(&date, &model.Model, &model.Input, &model.Output, &model.CacheRead,
			&model.CacheCreation, &model.Requests, &model.Tokens, &nano); err != nil {
			_ = rows.Close()
			return errors.New("cannot decode persisted model usage")
		}
		if nano.Valid {
			model.NanoAIU = new(int64)
			*model.NanoAIU = nano.Int64
		}
		if dayIndex, ok := byDate[date]; ok {
			key := struct{ date, model string }{date, model.Model}
			if target, exists := models[key]; exists {
				addStoredTotals(&days[target.day].Models[target.model].Totals, model.Totals)
			} else {
				days[dayIndex].Models = append(days[dayIndex].Models, model)
				models[key] = struct{ day, model int }{dayIndex, len(days[dayIndex].Models) - 1}
			}
		}
	}
	if err := rows.Close(); err != nil || rows.Err() != nil {
		return errors.New("cannot read persisted model usage")
	}
	costRows, err := tx.QueryContext(ctx, `SELECT c.date, c.model, c.currency, c.total_cost_nanos
	FROM daily_model_costs c
	JOIN daily_usage d ON d.segment_id = c.segment_id AND d.date = c.date
	JOIN usage_segments g ON g.id = c.segment_id
	WHERE g.account_id = ? AND d.start_ms >= ? AND d.start_ms < ?
	ORDER BY c.date, c.model, c.currency`, accountID, start, end)
	if err != nil {
		return errors.New("cannot read persisted model costs")
	}
	defer func() { _ = costRows.Close() }()
	for costRows.Next() {
		var date, model, currency string
		var nanos int64
		if err := costRows.Scan(&date, &model, &currency, &nanos); err != nil {
			return errors.New("cannot decode persisted model costs")
		}
		if target, ok := models[struct{ date, model string }{date, model}]; ok {
			addStoredCost(&days[target.day].Models[target.model].Costs, currency, nanos)
		}
	}
	if costRows.Err() != nil {
		return errors.New("cannot read persisted model costs")
	}
	for dayIndex := range days {
		sort.Slice(days[dayIndex].Models, func(i, j int) bool { return days[dayIndex].Models[i].Model < days[dayIndex].Models[j].Model })
		for modelIndex := range days[dayIndex].Models {
			costs := days[dayIndex].Models[modelIndex].Costs
			sort.Slice(costs, func(i, j int) bool { return costs[i].Currency < costs[j].Currency })
		}
	}
	return nil
}
