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

const schemaVersion = 1

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
		`CREATE TABLE IF NOT EXISTS daily_usage (
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
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
			PRIMARY KEY(account_id, date)
		)`,
		`CREATE TABLE IF NOT EXISTS daily_usage_costs (
			account_id INTEGER NOT NULL,
			date TEXT NOT NULL,
			currency TEXT NOT NULL,
			total_cost_nanos INTEGER NOT NULL,
			PRIMARY KEY(account_id, date, currency),
			FOREIGN KEY(account_id, date) REFERENCES daily_usage(account_id, date) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS daily_model_usage (
			account_id INTEGER NOT NULL,
			date TEXT NOT NULL,
			model TEXT NOT NULL,
			input_tokens INTEGER NOT NULL,
			output_tokens INTEGER NOT NULL,
			cache_read_input_tokens INTEGER NOT NULL,
			cache_creation_input_tokens INTEGER NOT NULL,
			request_count INTEGER NOT NULL,
			total_tokens INTEGER NOT NULL,
			total_nano_aiu INTEGER,
			PRIMARY KEY(account_id, date, model),
			FOREIGN KEY(account_id, date) REFERENCES daily_usage(account_id, date) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS daily_model_costs (
			account_id INTEGER NOT NULL,
			date TEXT NOT NULL,
			model TEXT NOT NULL,
			currency TEXT NOT NULL,
			total_cost_nanos INTEGER NOT NULL,
			PRIMARY KEY(account_id, date, model, currency),
			FOREIGN KEY(account_id, date, model) REFERENCES daily_model_usage(account_id, date, model) ON DELETE CASCADE
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

func (s *Store) SaveDaily(ctx context.Context, sourceType, name, normalizedURL, login string, daily *upstream.Daily) error {
	if daily == nil || daily.Range.EndMS <= 0 {
		return errors.New("invalid daily usage snapshot")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("cannot begin persistence transaction")
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UnixMilli()
	key := sourceKey(sourceType, name)
	if _, err := tx.ExecContext(ctx, `INSERT INTO sources(source_key, source_type, name, normalized_url, last_seen_ms)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(source_key) DO UPDATE SET source_type=excluded.source_type, name=excluded.name,
		normalized_url=excluded.normalized_url, last_seen_ms=excluded.last_seen_ms`, key, sourceType, name, normalizedURL, now); err != nil {
		return errors.New("cannot persist source")
	}
	var sourceID int64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM sources WHERE source_key = ?", key).Scan(&sourceID); err != nil {
		return errors.New("cannot resolve persisted source")
	}
	loginKey := strings.ToLower(login)
	if _, err := tx.ExecContext(ctx, `INSERT INTO accounts(source_id, login_key, login, first_seen_ms, last_seen_ms)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(source_id, login_key) DO UPDATE SET login=excluded.login, last_seen_ms=excluded.last_seen_ms`, sourceID, loginKey, login, now, now); err != nil {
		return errors.New("cannot persist account")
	}
	var accountID int64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM accounts WHERE source_id = ? AND login_key = ?", sourceID, loginKey).Scan(&accountID); err != nil {
		return errors.New("cannot resolve persisted account")
	}

	for _, day := range daily.Days {
		if err := replaceDay(ctx, tx, accountID, daily.Range.EndMS, day); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return errors.New("cannot commit persisted usage")
	}
	return nil
}

func replaceDay(ctx context.Context, tx *sql.Tx, accountID, snapshotEnd int64, day upstream.Day) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO daily_usage(
		account_id, date, start_ms, end_ms, snapshot_end_ms, input_tokens, output_tokens,
		cache_read_input_tokens, cache_creation_input_tokens, request_count,
		total_tokens, total_nano_aiu
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(account_id, date) DO UPDATE SET
		start_ms=excluded.start_ms, end_ms=excluded.end_ms, snapshot_end_ms=excluded.snapshot_end_ms,
		input_tokens=excluded.input_tokens, output_tokens=excluded.output_tokens,
		cache_read_input_tokens=excluded.cache_read_input_tokens,
		cache_creation_input_tokens=excluded.cache_creation_input_tokens,
		request_count=excluded.request_count, total_tokens=excluded.total_tokens,
		total_nano_aiu=excluded.total_nano_aiu
	WHERE excluded.snapshot_end_ms >= daily_usage.snapshot_end_ms`,
		accountID, day.Date, day.StartMS, day.EndMS, snapshotEnd, day.Totals.Input, day.Totals.Output,
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
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE account_id = ? AND date = ?", accountID, day.Date); err != nil {
			return errors.New("cannot replace persisted daily details")
		}
	}
	for _, cost := range day.Totals.Costs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO daily_usage_costs(account_id, date, currency, total_cost_nanos) VALUES(?, ?, ?, ?)`, accountID, day.Date, cost.Currency, cost.Nanos); err != nil {
			return errors.New("cannot persist daily cost")
		}
	}
	for _, model := range day.Models {
		if _, err := tx.ExecContext(ctx, `INSERT INTO daily_model_usage(
			account_id, date, model, input_tokens, output_tokens, cache_read_input_tokens,
			cache_creation_input_tokens, request_count, total_tokens, total_nano_aiu
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, accountID, day.Date, model.Model,
			model.Input, model.Output, model.CacheRead, model.CacheCreation, model.Requests,
			model.Tokens, nullableInt64(model.NanoAIU)); err != nil {
			return errors.New("cannot persist daily model usage")
		}
		for _, cost := range model.Costs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO daily_model_costs(account_id, date, model, currency, total_cost_nanos) VALUES(?, ?, ?, ?, ?)`, accountID, day.Date, model.Model, cost.Currency, cost.Nanos); err != nil {
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
	JOIN accounts a ON a.id = d.account_id
	JOIN sources s ON s.id = a.source_id
	WHERE s.source_key = ? AND a.login_key = ? AND d.start_ms >= ? AND d.start_ms < ?
	ORDER BY d.date`, sourceKey(sourceType, name), strings.ToLower(login), start, end)
	if err != nil {
		return nil, false, errors.New("cannot read persisted usage")
	}
	days := []upstream.Day{}
	byDate := map[string]int{}
	for rows.Next() {
		var day upstream.Day
		var nano sql.NullInt64
		day.Totals = &upstream.Totals{Costs: []upstream.Cost{}}
		day.Models = []upstream.Model{}
		if err := rows.Scan(&day.Date, &day.StartMS, &day.EndMS, &day.Totals.Input, &day.Totals.Output,
			&day.Totals.CacheRead, &day.Totals.CacheCreation, &day.Totals.Requests,
			&day.Totals.Tokens, &nano); err != nil {
			_ = rows.Close()
			return nil, false, errors.New("cannot decode persisted usage")
		}
		if nano.Valid {
			day.Totals.NanoAIU = new(int64)
			*day.Totals.NanoAIU = nano.Int64
		}
		days = append(days, day)
		byDate[day.Date] = len(days) - 1
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
	if err := s.loadCosts(ctx, tx, sourceType, name, login, period, now, days, byDate); err != nil {
		return nil, false, err
	}
	if err := s.loadModels(ctx, tx, sourceType, name, login, period, now, days, byDate); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, errors.New("cannot commit persistence read transaction")
	}
	return days, true, nil
}

func (s *Store) loadCosts(ctx context.Context, tx *sql.Tx, sourceType, name, login, period string, now time.Time, days []upstream.Day, byDate map[string]int) error {
	start, end := periodStart(period, now), now.UnixMilli()+1
	rows, err := tx.QueryContext(ctx, `SELECT c.date, c.currency, c.total_cost_nanos
	FROM daily_usage_costs c
	JOIN accounts a ON a.id = c.account_id JOIN sources s ON s.id = a.source_id
	JOIN daily_usage d ON d.account_id = c.account_id AND d.date = c.date
	WHERE s.source_key = ? AND a.login_key = ? AND d.start_ms >= ? AND d.start_ms < ?
	ORDER BY c.date, c.currency`, sourceKey(sourceType, name), strings.ToLower(login), start, end)
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
			days[index].Totals.Costs = append(days[index].Totals.Costs, upstream.Cost{Currency: currency, Nanos: nanos, Amount: float64(nanos) / 1e9})
		}
	}
	if rows.Err() != nil {
		return errors.New("cannot read persisted costs")
	}
	return nil
}

func (s *Store) loadModels(ctx context.Context, tx *sql.Tx, sourceType, name, login, period string, now time.Time, days []upstream.Day, byDate map[string]int) error {
	start, end := periodStart(period, now), now.UnixMilli()+1
	rows, err := tx.QueryContext(ctx, `SELECT m.date, m.model, m.input_tokens, m.output_tokens,
		m.cache_read_input_tokens, m.cache_creation_input_tokens, m.request_count,
		m.total_tokens, m.total_nano_aiu
	FROM daily_model_usage m
	JOIN accounts a ON a.id = m.account_id JOIN sources s ON s.id = a.source_id
	JOIN daily_usage d ON d.account_id = m.account_id AND d.date = m.date
	WHERE s.source_key = ? AND a.login_key = ? AND d.start_ms >= ? AND d.start_ms < ?
	ORDER BY m.date, m.model`, sourceKey(sourceType, name), strings.ToLower(login), start, end)
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
			days[dayIndex].Models = append(days[dayIndex].Models, model)
			models[struct{ date, model string }{date, model.Model}] = struct{ day, model int }{dayIndex, len(days[dayIndex].Models) - 1}
		}
	}
	if err := rows.Close(); err != nil || rows.Err() != nil {
		return errors.New("cannot read persisted model usage")
	}
	costRows, err := tx.QueryContext(ctx, `SELECT c.date, c.model, c.currency, c.total_cost_nanos
	FROM daily_model_costs c
	JOIN accounts a ON a.id = c.account_id JOIN sources s ON s.id = a.source_id
	JOIN daily_usage d ON d.account_id = c.account_id AND d.date = c.date
	WHERE s.source_key = ? AND a.login_key = ? AND d.start_ms >= ? AND d.start_ms < ?
	ORDER BY c.date, c.model, c.currency`, sourceKey(sourceType, name), strings.ToLower(login), start, end)
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
			days[target.day].Models[target.model].Costs = append(days[target.day].Models[target.model].Costs, upstream.Cost{Currency: currency, Nanos: nanos, Amount: float64(nanos) / 1e9})
		}
	}
	if costRows.Err() != nil {
		return errors.New("cannot read persisted model costs")
	}
	for _, day := range days {
		sort.Slice(day.Models, func(i, j int) bool { return day.Models[i].Model < day.Models[j].Model })
	}
	return nil
}
