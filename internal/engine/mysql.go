// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

package engine

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/indeclau/deitafix/internal/guard"
)

// MySQL es la implementación de Engine para MySQL/MariaDB, sobre
// database/sql + go-sql-driver/mysql.
type MySQL struct {
	db *sql.DB
}

// NewMySQL abre un pool contra el DSN dado (con el usuario restringido) y
// verifica la conectividad.
//
// Acepta tanto un DSN nativo del driver (user:pass@tcp(host:port)/db) como una
// URL mysql://user:pass@host:port/db, que normaliza al formato del driver.
func NewMySQL(ctx context.Context, dsn string) (*MySQL, error) {
	normalized, err := normalizeMySQLDSN(dsn)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("mysql", normalized)
	if err != nil {
		return nil, fmt.Errorf("mysql: abriendo pool: %w", err)
	}
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mysql: ping: %w", err)
	}
	return &MySQL{db: db}, nil
}

// Name identifica el motor.
func (m *MySQL) Name() string { return "mysql" }

// Parse clasifica una sentencia con el parser real de MySQL (TiDB).
func (m *MySQL) Parse(sql string) (guard.Statement, error) {
	return parseMySQL(sql)
}

// BuildSQL arma una sentencia parametrizada desde una operación acotada, con
// identificadores citados con backticks y placeholders ?.
func (m *MySQL) BuildSQL(op BoundedOp) (string, []any, error) {
	return buildBoundedSQL(op, quoteMySQL, placeholderQuestion)
}

// Preview ejecuta la sentencia en una transacción, mide las filas afectadas y
// hace ROLLBACK.
func (m *MySQL) Preview(ctx context.Context, query string, args ...any) (int64, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("mysql: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("mysql: exec en preview: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("mysql: rows affected: %w", err)
	}
	return affected, nil
}

// Confirm ejecuta la sentencia en una transacción y hace COMMIT.
func (m *MySQL) Confirm(ctx context.Context, query string, args ...any) (int64, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("mysql: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("mysql: exec en confirm: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("mysql: rows affected: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("mysql: commit: %w", err)
	}
	return affected, nil
}

// Ping verifica la conectividad con la base. Lo usa la probe de readiness.
func (m *MySQL) Ping(ctx context.Context) error {
	if err := m.db.PingContext(ctx); err != nil {
		return fmt.Errorf("mysql: ping: %w", err)
	}
	return nil
}

// Close cierra el pool.
func (m *MySQL) Close() error {
	return m.db.Close()
}

// Columns implementa engine.Introspector para MySQL/MariaDB: lee
// information_schema.columns acotado a la base actual (DATABASE()) y a las
// tablas pedidas (las de la whitelist), con un IN (?, ?, ...) parametrizado.
// Solo aparecen tablas visibles para el usuario restringido; no ejecuta ninguna
// escritura.
func (m *MySQL) Columns(ctx context.Context, tables []string) (map[string][]ColumnInfo, error) {
	out := make(map[string][]ColumnInfo, len(tables))
	if len(tables) == 0 {
		return out, nil
	}

	// Placeholders posicionales para el IN; los nombres de tabla van como args,
	// nunca interpolados.
	placeholders := make([]string, len(tables))
	args := make([]any, 0, len(tables))
	for i, t := range tables {
		placeholders[i] = "?"
		args = append(args, t)
	}

	// La única parte "dinámica" de la query es la lista de placeholders "?"
	// (uno por tabla), generada por el código de arriba; NO hay ningún valor de
	// entrada interpolado. Los nombres de tabla viajan como args parametrizados.
	// Por eso no hay inyección posible. database/sql no admite un slice como
	// parámetro de IN, de ahí la construcción del IN con N placeholders.
	q := "SELECT table_name, column_name, data_type " +
		"FROM information_schema.columns " +
		"WHERE table_schema = DATABASE() AND table_name IN (" +
		strings.Join(placeholders, ", ") + ") " +
		"ORDER BY table_name, ordinal_position"

	rows, err := m.db.QueryContext(ctx, q, args...) // NOSONAR go:S2077 — solo placeholders "?" fijos; nombres de tabla parametrizados, sin interpolación.
	if err != nil {
		return nil, fmt.Errorf("mysql: introspección de esquema: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var table, col, typ string
		if err := rows.Scan(&table, &col, &typ); err != nil {
			return nil, fmt.Errorf("mysql: leyendo esquema: %w", err)
		}
		out[table] = append(out[table], ColumnInfo{Name: col, Type: typ})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: iterando esquema: %w", err)
	}
	return out, nil
}

// normalizeMySQLDSN convierte una URL mysql://user:pass@host:port/db al DSN que
// espera go-sql-driver (user:pass@tcp(host:port)/db). Si ya es un DSN nativo,
// lo devuelve tal cual. En ambos casos fuerza parseTime para leer fechas.
func normalizeMySQLDSN(dsn string) (string, error) {
	if !strings.HasPrefix(dsn, "mysql://") {
		// Ya es un DSN nativo del driver: solo asegurar parseTime.
		cfg, err := mysql.ParseDSN(dsn)
		if err != nil {
			return "", fmt.Errorf("mysql: DSN inválido: %w", err)
		}
		cfg.ParseTime = true
		return cfg.FormatDSN(), nil
	}

	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("mysql: URL inválida: %w", err)
	}
	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = u.Host
	cfg.User = u.User.Username()
	if pw, ok := u.User.Password(); ok {
		cfg.Passwd = pw
	}
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	cfg.ParseTime = true
	return cfg.FormatDSN(), nil
}
