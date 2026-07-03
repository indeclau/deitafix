// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

// Package guard implementa las guardas de sentencia de Deitafix.
//
// Las guardas son la primera línea de defensa (la segunda es el usuario
// restringido de la base). Se apoyan en el parser real de cada motor —nunca en
// regex— para clasificar la sentencia y luego aplican reglas puras sobre esa
// clasificación:
//
//   - solo se permiten UPDATE, DELETE e INSERT;
//   - UPDATE y DELETE deben tener WHERE;
//   - INSERT solo con VALUES explícitos (se rechaza INSERT ... SELECT);
//   - se rechaza cualquier DDL / DROP / TRUNCATE / multi-statement;
//   - la tabla objetivo debe estar en la whitelist configurada.
//
// El checker (Check) es una función pura sobre un Statement ya parseado, por lo
// que es trivialmente testeable en tablas sin necesidad de una base de datos.
package guard

import (
	"errors"
	"fmt"
)

// Operation es el tipo de sentencia de escritura soportado.
type Operation string

const (
	OpInsert Operation = "INSERT"
	OpUpdate Operation = "UPDATE"
	OpDelete Operation = "DELETE"
)

// Statement es la representación neutral al motor de una sentencia ya parseada.
// Los parsers concretos (postgres, mysql) producen este tipo; el checker opera
// solo sobre él, sin conocer el motor.
type Statement struct {
	// Op es la operación de escritura.
	Op Operation

	// Table es el nombre de la tabla objetivo, tal cual aparece en la
	// sentencia (sin comillas, con su casing original).
	Table string

	// HasWhere indica si la sentencia tiene una cláusula WHERE. Solo es
	// relevante para UPDATE y DELETE.
	HasWhere bool

	// InsertFromSelect es true cuando la sentencia es un INSERT ... SELECT
	// (en vez de INSERT ... VALUES). Solo relevante para INSERT.
	InsertFromSelect bool
}

// Errores centinela para que los callers puedan distinguir el motivo del
// rechazo (por ejemplo, la capa HTTP mapea cada uno a un mensaje claro).
var (
	// ErrEmptySQL indica que no se recibió ninguna sentencia.
	ErrEmptySQL = errors.New("guard: sentencia vacía")

	// ErrMultipleStatements indica que se recibió más de una sentencia.
	ErrMultipleStatements = errors.New("guard: se permite exactamente una sentencia")

	// ErrParse indica que el motor no pudo parsear la sentencia.
	ErrParse = errors.New("guard: no se pudo parsear la sentencia")

	// ErrOperationNotAllowed indica una operación fuera de la whitelist de
	// operaciones (por ejemplo SELECT, DDL, DROP, TRUNCATE).
	ErrOperationNotAllowed = errors.New("guard: operación no permitida")

	// ErrMissingWhere indica un UPDATE o DELETE sin cláusula WHERE.
	ErrMissingWhere = errors.New("guard: UPDATE/DELETE requiere WHERE")

	// ErrInsertFromSelect indica un INSERT ... SELECT (no soportado en v1).
	ErrInsertFromSelect = errors.New("guard: INSERT solo admite VALUES explícitos")

	// ErrTableNotWhitelisted indica que la tabla objetivo no está en la
	// whitelist configurada.
	ErrTableNotWhitelisted = errors.New("guard: tabla fuera de la whitelist")

	// ErrRowsExceeded indica que el impacto medido supera MAX_AFFECTED_ROWS.
	ErrRowsExceeded = errors.New("guard: se supera el tope de filas afectadas")
)

// Check aplica las guardas puras sobre un Statement ya parseado, contra la
// whitelist de tablas provista.
//
// La whitelist se pasa explícitamente (en vez de leerse de config global) para
// mantener la función pura y fácil de testear. Si whitelist está vacía, ninguna
// tabla se considera permitida (default seguro).
func Check(stmt Statement, whitelist []string) error {
	switch stmt.Op {
	case OpUpdate, OpDelete:
		if !stmt.HasWhere {
			return ErrMissingWhere
		}
	case OpInsert:
		if stmt.InsertFromSelect {
			return ErrInsertFromSelect
		}
	default:
		return fmt.Errorf("%w: %q", ErrOperationNotAllowed, stmt.Op)
	}

	if !tableAllowed(stmt.Table, whitelist) {
		return fmt.Errorf("%w: %q", ErrTableNotWhitelisted, stmt.Table)
	}

	return nil
}

// CheckAffectedRows verifica el tope de filas medido en el preview.
//
// Es una guarda aparte porque el número de filas afectadas solo se conoce tras
// ejecutar la sentencia dentro de la transacción de preview, no en el parseo.
func CheckAffectedRows(affected, maxRows int64) error {
	if affected > int64(maxRows) {
		return fmt.Errorf("%w: %d > %d", ErrRowsExceeded, affected, maxRows)
	}
	return nil
}

// tableAllowed compara la tabla objetivo contra la whitelist. La comparación es
// exacta y sensible a mayúsculas: Postgres distingue "CollectionBox" de
// collectionbox, y una whitelist laxa sería un agujero de seguridad.
func tableAllowed(table string, whitelist []string) bool {
	for _, t := range whitelist {
		if t == table {
			return true
		}
	}
	return false
}
