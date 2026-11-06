// Package store persists burnrate data in SQLite using parameterized queries only.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/Mbarney928/burnrate/internal/model"
	"github.com/Mbarney928/burnrate/internal/slo"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is the SQLite-backed repository for services, SLOs, events and API tokens.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open connects to the SQLite database at dbPath, creating the file if needed.
func Open(ctx context.Context, dbPath string) (*Store, error) {
	dsn := "file:" + dbPath + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// SQLite allows one writer at a time; a single connection avoids SQLITE_BUSY under load
	// and is plenty for this workload.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database is reachable; used by the health check.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Migrate applies any embedded migrations that have not run yet and returns how many it applied.
func (s *Store) Migrate(ctx context.Context) (int, error) {
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return 0, fmt.Errorf("create schema_migrations: %w", err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return 0, err
	}
	slices.Sort(names)

	applied := 0
	for _, name := range names {
		version := strings.TrimSuffix(path.Base(name), ".sql")
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n); err != nil {
			return applied, err
		}
		if n > 0 {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return applied, err
		}
		if err := s.inTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, s.now().Unix())
			return err
		}); err != nil {
			return applied, fmt.Errorf("migration %s: %w", version, err)
		}
		applied++
	}
	return applied, nil
}

// CreateService inserts svc and returns it with its ID and creation time set.
func (s *Store) CreateService(ctx context.Context, svc model.Service) (model.Service, error) {
	svc.CreatedAt = s.now().UTC().Truncate(time.Second)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO services (name, team, description, created_at) VALUES (?, ?, ?, ?)`,
		svc.Name, svc.Team, svc.Description, svc.CreatedAt.Unix())
	if err != nil {
		return model.Service{}, mapErr(err)
	}
	svc.ID, err = res.LastInsertId()
	return svc, err
}

// ServiceByName returns the service called name, or model.ErrNotFound.
func (s *Store) ServiceByName(ctx context.Context, name string) (model.Service, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, team, description, created_at FROM services WHERE name = ?`, name)
	return scanService(row)
}

// ListServices returns every service ordered by name.
func (s *Store) ListServices(ctx context.Context) ([]model.Service, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, team, description, created_at FROM services ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []model.Service{}
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

// CreateSLO inserts o and returns it with its ID and creation time set.
func (s *Store) CreateSLO(ctx context.Context, o model.SLO) (model.SLO, error) {
	o.CreatedAt = s.now().UTC().Truncate(time.Second)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO slos (service_id, name, target, window_days, created_at) VALUES (?, ?, ?, ?, ?)`,
		o.ServiceID, o.Name, o.Target, o.WindowDays, o.CreatedAt.Unix())
	if err != nil {
		return model.SLO{}, mapErr(err)
	}
	o.ID, err = res.LastInsertId()
	return o, err
}

const sloColumns = `SELECT o.id, o.service_id, s.name, o.name, o.target, o.window_days, o.created_at
	FROM slos o JOIN services s ON s.id = o.service_id`

// SLOByID returns the SLO with the given ID, or model.ErrNotFound.
func (s *Store) SLOByID(ctx context.Context, id int64) (model.SLO, error) {
	return scanSLO(s.db.QueryRowContext(ctx, sloColumns+` WHERE o.id = ?`, id))
}

// ListSLOs returns the SLOs of one service, or of every service when serviceID is 0.
func (s *Store) ListSLOs(ctx context.Context, serviceID int64) ([]model.SLO, error) {
	rows, err := s.db.QueryContext(ctx,
		sloColumns+` WHERE (? = 0 OR o.service_id = ?) ORDER BY s.name, o.name`, serviceID, serviceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []model.SLO{}
	for rows.Next() {
		o, err := scanSLO(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// AddEvents adds each bucket's counts to the stored minute bucket, atomically.
func (s *Store) AddEvents(ctx context.Context, sloID int64, buckets []slo.Bucket) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO event_buckets (slo_id, minute, good, total) VALUES (?, ?, ?, ?)
			ON CONFLICT (slo_id, minute) DO UPDATE SET good = good + excluded.good, total = total + excluded.total`)
		if err != nil {
			return err
		}
		defer func() { _ = stmt.Close() }()
		for _, b := range buckets {
			minute := b.Start.Truncate(time.Minute).Unix()
			if _, err := stmt.ExecContext(ctx, sloID, minute, b.Good, b.Total); err != nil {
				return mapErr(err)
			}
		}
		return nil
	})
}

// Buckets returns the minute buckets of an SLO whose start lies in (from, to], oldest first.
func (s *Store) Buckets(ctx context.Context, sloID int64, from, to time.Time) ([]slo.Bucket, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT minute, good, total FROM event_buckets WHERE slo_id = ? AND minute > ? AND minute <= ? ORDER BY minute`,
		sloID, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []slo.Bucket
	for rows.Next() {
		var minute int64
		var b slo.Bucket
		if err := rows.Scan(&minute, &b.Good, &b.Total); err != nil {
			return nil, err
		}
		b.Start = time.Unix(minute, 0).UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}

// CreateToken stores a token's name, role and secret hash.
func (s *Store) CreateToken(ctx context.Context, name string, role model.Role, hash string) (model.Token, error) {
	t := model.Token{Name: name, Role: role, CreatedAt: s.now().UTC().Truncate(time.Second)}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO api_tokens (name, role, token_hash, created_at) VALUES (?, ?, ?, ?)`,
		name, string(role), hash, t.CreatedAt.Unix())
	if err != nil {
		return model.Token{}, mapErr(err)
	}
	t.ID, err = res.LastInsertId()
	return t, err
}

// TokenByHash returns the token whose secret hashes to hash, or model.ErrNotFound.
func (s *Store) TokenByHash(ctx context.Context, hash string) (model.Token, error) {
	var t model.Token
	var role string
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, role, created_at FROM api_tokens WHERE token_hash = ?`, hash).
		Scan(&t.ID, &t.Name, &role, &created)
	if err != nil {
		return model.Token{}, mapErr(err)
	}
	t.Role = model.Role(role)
	t.CreatedAt = time.Unix(created, 0).UTC()
	return t, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanService(row scanner) (model.Service, error) {
	var svc model.Service
	var created int64
	if err := row.Scan(&svc.ID, &svc.Name, &svc.Team, &svc.Description, &created); err != nil {
		return model.Service{}, mapErr(err)
	}
	svc.CreatedAt = time.Unix(created, 0).UTC()
	return svc, nil
}

func scanSLO(row scanner) (model.SLO, error) {
	var o model.SLO
	var created int64
	if err := row.Scan(&o.ID, &o.ServiceID, &o.ServiceName, &o.Name, &o.Target, &o.WindowDays, &created); err != nil {
		return model.SLO{}, mapErr(err)
	}
	o.CreatedAt = time.Unix(created, 0).UTC()
	return o, nil
}

func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// mapErr translates driver errors into the domain's sentinel errors.
func mapErr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return model.ErrNotFound
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return fmt.Errorf("%w", model.ErrConflict)
		case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
			return fmt.Errorf("%w", model.ErrNotFound)
		}
	}
	return err
}
