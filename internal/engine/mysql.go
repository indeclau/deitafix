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
