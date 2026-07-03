// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/indeclau/deitafix/internal/guard"
)

// placeholderStyle describe cómo cada motor representa un parámetro en la
// sentencia: Postgres usa $1, $2, ...; MySQL usa ? posicional.
type placeholderStyle int

const (
	placeholderDollar   placeholderStyle = iota // $1, $2 (Postgres)
	placeholderQuestion                         // ? (MySQL)
)

// buildBoundedSQL construye una sentencia parametrizada a partir de una
// operación acotada, con los identificadores citados según el motor.
//
// Es una función pura (sin tocar la base) para poder testearla en tablas. El
// quoting de identificadores previene inyección vía nombres de tabla/columna;
// los valores viajan como placeholders, nunca interpolados.
//
// Reglas:
//   - UPDATE requiere Set y Where no vacíos.
//   - DELETE requiere Where no vacío (nunca un DELETE sin WHERE).
//   - Las columnas se ordenan alfabéticamente para que la salida sea
//     determinista y testeable.
func buildBoundedSQL(op BoundedOp, quote func(string) string, ph placeholderStyle) (string, []any, error) {
	if strings.TrimSpace(op.Table) == "" {
		return "", nil, fmt.Errorf("engine: operación acotada sin tabla")
	}
	if len(op.Where) == 0 {
		// Guarda estructural: una operación acotada nunca puede quedar sin WHERE.
		return "", nil, guard.ErrMissingWhere
	}

	var (
		sb   strings.Builder
		args []any
		n    int // contador de placeholders para el estilo $N
	)

	next := func() string {
		n++
		if ph == placeholderDollar {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}

	switch op.Op {
	case guard.OpUpdate:
		if len(op.Set) == 0 {
			return "", nil, fmt.Errorf("engine: UPDATE acotado sin campos SET")
		}
		sb.WriteString("UPDATE ")
		sb.WriteString(quote(op.Table))
		sb.WriteString(" SET ")
		for i, col := range sortedKeys(op.Set) {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(quote(col))
			sb.WriteString(" = ")
			sb.WriteString(next())
			args = append(args, op.Set[col])
		}
	case guard.OpDelete:
		sb.WriteString("DELETE FROM ")
		sb.WriteString(quote(op.Table))
	default:
		return "", nil, fmt.Errorf("%w: %q en modo acotado", guard.ErrOperationNotAllowed, op.Op)
	}

	sb.WriteString(" WHERE ")
	for i, col := range sortedKeys(op.Where) {
		if i > 0 {
			sb.WriteString(" AND ")
		}
		sb.WriteString(quote(col))
		sb.WriteString(" = ")
		sb.WriteString(next())
		args = append(args, op.Where[col])
	}

	return sb.String(), args, nil
}

// sortedKeys devuelve las claves de un mapa ordenadas alfabéticamente, para una
// salida SQL determinista.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// quotePostgres cita un identificador para Postgres con comillas dobles,
// escapando comillas embebidas. Preserva el casing (Postgres es sensible a
// mayúsculas cuando el identificador va citado).
func quotePostgres(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// quoteMySQL cita un identificador para MySQL/MariaDB con backticks, escapando
// backticks embebidos.
func quoteMySQL(ident string) string {
	return "`" + strings.ReplaceAll(ident, "`", "``") + "`"
}
