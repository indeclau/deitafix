// Package engine abstrae el motor de base de datos detrás de una interfaz
// común, con dos implementaciones: postgres (pgx) y mysql (go-sql-driver).
//
// Cada Engine encapsula tres responsabilidades:
//
//   - Parse: usar el parser real del motor para clasificar una sentencia en un
//     guard.Statement neutral (lo consume el checker de guardas).
//   - BuildSQL: construir de forma segura una sentencia a partir de una
//     operación acotada (tabla + campos + where), con identificadores citados.
//   - Preview / Confirm: ejecutar la sentencia dentro de una transacción. El
//     preview siempre hace ROLLBACK (mide sin persistir); el confirm hace
//     COMMIT.
package engine

import (
	"context"

	"github.com/indeclau/deitafix/internal/guard"
)

// Engine es la abstracción del motor de base de datos.
type Engine interface {
	// Name devuelve el identificador del motor ("postgres" | "mysql").
	Name() string

	// Parse clasifica una sentencia SQL usando el parser real del motor.
	// Devuelve un guard.Statement neutral o un error de parseo. No ejecuta
	// nada contra la base.
	Parse(sql string) (guard.Statement, error)

	// BuildSQL construye una sentencia SQL a partir de una operación acotada,
	// citando los identificadores para evitar inyección por nombres de tabla o
	// columna. Los valores van como placeholders en args.
	BuildSQL(op BoundedOp) (sql string, args []any, err error)

	// Preview ejecuta la sentencia dentro de una transacción, mide las filas
	// afectadas y hace ROLLBACK. Nada se persiste: es solo para medir impacto.
	Preview(ctx context.Context, sql string, args ...any) (affected int64, err error)

	// Confirm ejecuta la sentencia dentro de una transacción y hace COMMIT.
	// Devuelve las filas realmente afectadas.
	Confirm(ctx context.Context, sql string, args ...any) (affected int64, err error)

	// Close libera los recursos del pool de conexiones.
	Close() error
}

// BoundedOp describe una operación acotada: el segundo modo de entrada, en el
// que el cliente no manda SQL crudo sino una operación estructurada que el
// servicio traduce a SQL de forma segura.
//
// Solo se soportan UPDATE y DELETE en este modo (INSERT y casos más complejos
// van por SQL crudo). Un UPDATE requiere Set no vacío; un DELETE lo ignora.
type BoundedOp struct {
	// Op es la operación: guard.OpUpdate o guard.OpDelete.
	Op guard.Operation

	// Table es la tabla objetivo (se valida luego contra la whitelist).
	Table string

	// Set son los pares columna→valor a asignar (solo UPDATE).
	Set map[string]any

	// Where son los pares columna→valor de la cláusula WHERE, unidos con AND.
	// Debe ser no vacío: una operación acotada nunca puede quedar sin WHERE.
	Where map[string]any
}
