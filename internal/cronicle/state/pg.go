package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/stdlib"
)

// schemaFromDSN returns the per-deployment schema pinned via the DSN's
// search_path (first entry if a list), or "" for none. The producer
// self-creates this schema on connect so the control plane doesn't need
// DB access — it just hands over a DSN scoped to the deployment.
func schemaFromDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	sp := u.Query().Get("search_path")
	if i := strings.IndexByte(sp, ','); i >= 0 {
		sp = sp[:i]
	}
	return strings.TrimSpace(sp)
}

// quoteIdent double-quotes a Postgres identifier (schema name from the DSN).
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// dialect selects SQL flavor. The state store was written for SQLite; the
// Postgres path reuses the same queries with a thin placeholder-rebind
// driver + a DDL transform, so there's one schema/query source of truth.
type dialect int

const (
	dialectSQLite dialect = iota
	dialectPostgres
)

func isPostgresDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// ---- placeholder-rebind driver ---------------------------------------------
//
// All Store queries use `?` placeholders (SQLite). Postgres wants `$1,$2,…`.
// Rather than rewrite ~70 query sites, we register a tiny driver that wraps
// pgx's stdlib driver and rebinds `?`→`$N` at Prepare time. Every query in
// database/sql funnels through Prepare, so this covers exec/query/tx alike.

const pgxRebindDriverName = "cronicle-pgx-rebind"

var registerPGOnce sync.Once

func registerPGDriver() {
	registerPGOnce.Do(func() {
		sql.Register(pgxRebindDriverName, rebindDriver{base: stdlib.GetDefaultDriver()})
	})
}

type rebindDriver struct{ base driver.Driver }

func (d rebindDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &rebindConn{Conn: c}, nil
}

// rebindConn overrides only Prepare; Close/Begin are promoted from the
// embedded driver.Conn. database/sql falls back to prepared statements for
// Exec/Query when QueryerContext/ExecerContext aren't implemented, so every
// statement gets rebound. (We trade the direct-exec fast path for not having
// to touch every call site — fine at cronicle's load.)
type rebindConn struct{ driver.Conn }

func (c *rebindConn) Prepare(query string) (driver.Stmt, error) {
	return c.Conn.Prepare(rebindQuery(query))
}

// PrepareContext preserves the context (statement cancellation) while
// rebinding. database/sql routes Query/ExecContext through this when the
// conn doesn't implement Queryer/ExecerContext (we don't), so every
// statement is both context-aware and rebound.
func (c *rebindConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if cp, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return cp.PrepareContext(ctx, rebindQuery(query))
	}
	return c.Conn.Prepare(rebindQuery(query))
}

// BeginTx forwards to pgx's tx (which honors isolation levels) — the
// queue's atomic claim uses a non-default isolation level, which the
// deprecated driver.Conn.Begin can't express.
func (c *rebindConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if ct, ok := c.Conn.(driver.ConnBeginTx); ok {
		return ct.BeginTx(ctx, opts)
	}
	return c.Conn.Begin()
}

// rebindQuery converts `?` placeholders to `$1,$2,…`, skipping any `?`
// inside single-quoted string literals.
func rebindQuery(q string) string {
	if !strings.ContainsRune(q, '?') {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	inQuote := false
	for i := 0; i < len(q); i++ {
		ch := q[i]
		switch {
		case ch == '\'':
			inQuote = !inQuote
			b.WriteByte(ch)
		case ch == '?' && !inQuote:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// ---- DDL dialect transform -------------------------------------------------

// dialectizePG rewrites the SQLite DDL constants into Postgres-compatible
// form. The differences are systematic and the DDL is fully controlled (no
// user input), so targeted string replacement is safe.
func dialectizePG(ddl string) string {
	r := strings.NewReplacer(
		// AUTOINCREMENT integer PK → identity column.
		"INTEGER PRIMARY KEY AUTOINCREMENT", "BIGSERIAL PRIMARY KEY",
		// CURRENT_TIMESTAMP is timestamptz in PG; the columns are TEXT, so
		// cast on default.
		"DEFAULT (CURRENT_TIMESTAMP)", "DEFAULT (CURRENT_TIMESTAMP::text)",
		// SQLite BLOB → PG BYTEA (secrets value_ct/value_nonce).
		" BLOB", " BYTEA",
	)
	return r.Replace(ddl)
}

// splitStatements breaks a multi-statement DDL block into individual
// statements. pgx's extended protocol rejects multi-statement Exec, and
// splitting is harmless for SQLite too — so migrate runs every statement
// individually regardless of dialect. The state DDL has no `;` inside
// string literals, so a naive split is correct here.
func splitStatements(block string) []string {
	var out []string
	var cur strings.Builder
	inLineComment := false // inside `-- … \n`
	inQuote := false       // inside '…'
	for i := 0; i < len(block); i++ {
		ch := block[i]
		switch {
		case inLineComment:
			cur.WriteByte(ch)
			if ch == '\n' {
				inLineComment = false
			}
		case inQuote:
			cur.WriteByte(ch)
			if ch == '\'' {
				inQuote = false
			}
		case ch == '-' && i+1 < len(block) && block[i+1] == '-':
			inLineComment = true
			cur.WriteByte(ch)
		case ch == '\'':
			inQuote = true
			cur.WriteByte(ch)
		case ch == ';':
			if strings.TrimSpace(cur.String()) != "" {
				out = append(out, cur.String())
			}
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}
