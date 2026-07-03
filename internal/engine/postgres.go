package engine

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/indeclau/deitafix/internal/guard"
)

// Postgres es la implementación de Engine para PostgreSQL, sobre pgx.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres abre un pool de conexiones contra la URL dada (con el usuario
// restringido) y verifica la conectividad con un ping.
func NewPostgres(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("postgres: abriendo pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

// Name identifica el motor.
func (p *Postgres) Name() string { return "postgres" }

// Parse clasifica una sentencia con el parser real de Postgres.
func (p *Postgres) Parse(sql string) (guard.Statement, error) {
	return parsePostgres(sql)
}

// BuildSQL arma una sentencia parametrizada desde una operación acotada, con
// identificadores citados al estilo Postgres y placeholders $N.
func (p *Postgres) BuildSQL(op BoundedOp) (string, []any, error) {
	return buildBoundedSQL(op, quotePostgres, placeholderDollar)
}

// Preview ejecuta la sentencia en una transacción, mide las filas afectadas y
// hace ROLLBACK: no persiste nada, solo mide el impacto.
func (p *Postgres) Preview(ctx context.Context, sql string, args ...any) (int64, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("postgres: begin: %w", err)
	}
	// El rollback siempre corre: si Commit no se llama (que es el caso del
	// preview), deshace todo. Tras un Commit exitoso, el Rollback es no-op.
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return 0, fmt.Errorf("postgres: exec en preview: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Confirm ejecuta la sentencia en una transacción y hace COMMIT.
func (p *Postgres) Confirm(ctx context.Context, sql string, args ...any) (int64, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return 0, fmt.Errorf("postgres: exec en confirm: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("postgres: commit: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Close cierra el pool.
func (p *Postgres) Close() error {
	p.pool.Close()
	return nil
}
