package configsource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresSource pulls cronicle.hcl from a row in a Postgres table.
// Designed to be compatible with cronicle-infra's `schedule_configs`
// table out of the box; the URL query string parameterizes the
// table/column/key names so other deployments can adopt it without
// schema lock-in.
//
// URL shape:
//
//	postgres://user:pass@host:5432/db?
//	    table=schedule_configs&
//	    key_col=project_id&
//	    value_col=hcl_source&
//	    version_col=version&        (optional — used as etag if present)
//	    key=demo&                   (the row's primary key value)
//	    sslmode=disable             (passed through to libpq)
//
// Defaults (mirroring cronicle-infra):
//   - table       = schedule_configs
//   - key_col     = project_id
//   - value_col   = hcl_source
//   - version_col = version       (etag source; pass version_col= to disable)
//   - key         = (required; no default)
//
// Etag: when version_col is set, the row's version becomes the etag.
// Sub-second refreshes only do a single column read per tick. Without
// version_col, we fall back to sha256 of the value content (still
// avoids the parse step on unchanged content but does a full row read).
//
// Note for cronicle-infra users: cronicle-infra applies a pause filter
// (rewriting cron = "" for paused schedules) at the HTTP serve layer.
// Reading directly from Postgres skips that — paused schedules will
// fire. For full cronicle-infra semantics, use HTTPSource pointed at
// /v1/projects/{p}/cronicle.hcl instead.
type PostgresSource struct {
	pool       *pgxpool.Pool
	table      string
	keyCol     string
	valueCol   string
	versionCol string // empty disables version-based etag
	key        string

	display string
}

// NewPostgresSource parses a postgres:// URL (also accepts postgresql://)
// and connects a pool. The pool stays open for the source's lifetime;
// call Close on shutdown.
func NewPostgresSource(ctx context.Context, rawurl string) (*PostgresSource, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, fmt.Errorf("invalid postgres url: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return nil, fmt.Errorf("not a postgres url: %s", rawurl)
	}

	q := u.Query()
	table := orDefault(q.Get("table"), "schedule_configs")
	keyCol := orDefault(q.Get("key_col"), "project_id")
	valueCol := orDefault(q.Get("value_col"), "hcl_source")
	versionCol := q.Get("version_col") // may be empty (default "version", but unset means: hash-based etag)
	if _, ok := q["version_col"]; !ok {
		versionCol = "version"
	}
	key := q.Get("key")
	if key == "" {
		return nil, fmt.Errorf("postgres source url must include ?key=<row-key>")
	}
	if err := validateSQLIdent(table, "table"); err != nil {
		return nil, err
	}
	if err := validateSQLIdent(keyCol, "key_col"); err != nil {
		return nil, err
	}
	if err := validateSQLIdent(valueCol, "value_col"); err != nil {
		return nil, err
	}
	if versionCol != "" {
		if err := validateSQLIdent(versionCol, "version_col"); err != nil {
			return nil, err
		}
	}

	// Strip configsource-specific query params before handing to pgx
	// so libpq doesn't choke on unknown options like ?table=…
	stripped := *u
	cleaned := url.Values{}
	for k, vs := range q {
		switch k {
		case "table", "key_col", "value_col", "version_col", "key":
			continue
		default:
			cleaned[k] = vs
		}
	}
	stripped.RawQuery = cleaned.Encode()

	pool, err := pgxpool.New(ctx, stripped.String())
	if err != nil {
		return nil, fmt.Errorf("postgres source: pool: %w", err)
	}

	// Redact userinfo for the display string.
	display := stripped
	display.User = nil
	display.RawQuery = "" // hide connection options from logs
	return &PostgresSource{
		pool:       pool,
		table:      table,
		keyCol:     keyCol,
		valueCol:   valueCol,
		versionCol: versionCol,
		key:        key,
		display:    fmt.Sprintf("%s?table=%s&key=%s", display.String(), table, key),
	}, nil
}

// Close releases the connection pool. Safe to call multiple times.
func (s *PostgresSource) Close() error {
	if s.pool != nil {
		s.pool.Close()
		s.pool = nil
	}
	return nil
}

func (s *PostgresSource) Fetch(ctx context.Context, prevEtag string) ([]byte, string, bool, error) {
	if s.versionCol != "" {
		// Fast path: probe just the version column. If it matches the
		// caller's etag, no need to pull the full HCL body.
		var version any
		err := s.pool.QueryRow(ctx,
			fmt.Sprintf("SELECT %s FROM %s WHERE %s = $1",
				quoteIdent(s.versionCol), quoteIdent(s.table), quoteIdent(s.keyCol)),
			s.key,
		).Scan(&version)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, prevEtag, false, ErrNotFound
			}
			return nil, prevEtag, false, fmt.Errorf("postgres source: version probe: %w", err)
		}
		etag := fmt.Sprintf("%v", version)
		if etag == prevEtag {
			return nil, etag, false, nil
		}
		var body []byte
		err = s.pool.QueryRow(ctx,
			fmt.Sprintf("SELECT %s FROM %s WHERE %s = $1",
				quoteIdent(s.valueCol), quoteIdent(s.table), quoteIdent(s.keyCol)),
			s.key,
		).Scan(&body)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, prevEtag, false, ErrNotFound
			}
			return nil, prevEtag, false, fmt.Errorf("postgres source: select value: %w", err)
		}
		return body, etag, true, nil
	}

	// No version column: pull the value, hash it for the etag.
	var body []byte
	err := s.pool.QueryRow(ctx,
		fmt.Sprintf("SELECT %s FROM %s WHERE %s = $1",
			quoteIdent(s.valueCol), quoteIdent(s.table), quoteIdent(s.keyCol)),
		s.key,
	).Scan(&body)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, prevEtag, false, ErrNotFound
		}
		return nil, prevEtag, false, fmt.Errorf("postgres source: select value: %w", err)
	}
	sum := sha256.Sum256(body)
	etag := hex.EncodeToString(sum[:])
	if etag == prevEtag {
		return nil, etag, false, nil
	}
	return body, etag, true, nil
}

func (s *PostgresSource) String() string {
	return s.display
}

// validateSQLIdent rejects any identifier that isn't a plain
// alphanumeric/underscore string — we splice these into the query
// directly (pgx parameterizes values, not identifiers). Anything
// rejected here would either fail to parse or open an injection path.
func validateSQLIdent(s, what string) error {
	if s == "" {
		return fmt.Errorf("postgres source: %s cannot be empty", what)
	}
	for i, r := range s {
		if r == '_' {
			continue
		}
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '0' && r <= '9' && i > 0 {
			continue
		}
		return fmt.Errorf("postgres source: %s contains invalid character %q (allowed: alnum + underscore, must not start with digit)",
			what, r)
	}
	return nil
}

// quoteIdent wraps a validated identifier in double quotes so the
// query handles mixed-case table/column names correctly. Safe because
// validateSQLIdent has already excluded the quote character.
func quoteIdent(s string) string {
	return `"` + s + `"`
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
