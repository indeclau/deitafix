// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

package engine

import (
	"errors"
	"testing"

	"github.com/indeclau/deitafix/internal/guard"
)

// Estos tests fijan el comportamiento de seguridad de los parsers REALES de cada
// motor (pg_query_go para Postgres, TiDB para MySQL). Son la evidencia de que
// cada bypass conocido se detecta a nivel de parseo —no de regex— antes de que
// la sentencia llegue siquiera a tocar la base.
//
// Cada caso se ejecuta contra AMBOS parsers salvo que sea específico de un
// dialecto. Se afirma sobre el guard.Statement resultante (o el error), no sobre
// texto: es exactamente lo que el checker (guard.Check) consume después.
//
// Nota sobre el casing de la tabla: PostgreSQL "foldea" los identificadores sin
// comillas a minúscula (CollectionBox -> collectionbox); MySQL/MariaDB los
// preserva. Para que los casos genéricos sean deterministas en ambos motores se
// usa una tabla en minúscula (collectionbox), que matchea el folding de Postgres
// y una whitelist en minúscula. El folding en sí se fija aparte, en
// TestPostgresFoldsUnquotedIdentifiers, por ser una propiedad de seguridad
// relevante (la whitelist debe escribirse coherente con cómo cada motor
// normaliza el nombre).

// parseFn empareja un nombre de motor con su parser package-level.
type parseFn struct {
	name  string
	parse func(string) (guard.Statement, error)
}

// bothParsers son los dos parsers reales. parsePostgres usa cgo (libpg_query);
// en el CI el runner tiene el toolchain de C. parseMySQL (TiDB) es Go puro.
func bothParsers() []parseFn {
	return []parseFn{
		{"postgres", parsePostgres},
		{"mysql", parseMySQL},
	}
}

// securityCase describe una sentencia de entrada y lo que se espera del parseo.
type securityCase struct {
	name string
	sql  string

	// wantErr, si no es nil, es el error centinela que se espera (se compara con
	// errors.Is). Si es nil, se espera un parseo exitoso que produce wantStmt.
	wantErr error

	// wantStmt es el Statement esperado cuando wantErr es nil. Se compara campo a
	// campo (Op, Table, HasWhere, InsertFromSelect).
	wantStmt guard.Statement

	// onlyEngine, si no está vacío, restringe el caso a un solo motor (para SQL
	// específico de un dialecto, p.ej. WHERE true de Postgres).
	onlyEngine string
}

// TestParsersRejectBypasses recorre los bypasses conocidos y verifica que cada
// parser real los clasifica de forma que el checker los rechace, o que produce
// exactamente el Statement esperado cuando la sentencia es legítima.
func TestParsersRejectBypasses(t *testing.T) {
	cases := []securityCase{
		// --- UPDATE/DELETE sin WHERE ---
		// El parser marca HasWhere=false; el checker lo rechaza con ErrMissingWhere.
		{
			name:     "UPDATE sin WHERE",
			sql:      "UPDATE collectionbox SET amount = 0",
			wantStmt: guard.Statement{Op: guard.OpUpdate, Table: "collectionbox", HasWhere: false},
		},
		{
			name:     "DELETE sin WHERE",
			sql:      "DELETE FROM collectionbox",
			wantStmt: guard.Statement{Op: guard.OpDelete, Table: "collectionbox", HasWhere: false},
		},

		// --- Comentarios que ocultan la intención ---
		// Un WHERE comentado NO es un WHERE: el parser real ve que no existe.
		{
			name:     "WHERE comentado con -- no cuenta como WHERE",
			sql:      "UPDATE collectionbox SET amount = 0 -- WHERE id = 1",
			wantStmt: guard.Statement{Op: guard.OpUpdate, Table: "collectionbox", HasWhere: false},
		},
		{
			name:     "WHERE comentado con /* */ no cuenta como WHERE",
			sql:      "DELETE FROM collectionbox /* WHERE id = 1 */",
			wantStmt: guard.Statement{Op: guard.OpDelete, Table: "collectionbox", HasWhere: false},
		},
		{
			name:     "comentario embebido no oculta la ausencia de WHERE",
			sql:      "UPDATE /* sneaky */ collectionbox SET amount = 0",
			wantStmt: guard.Statement{Op: guard.OpUpdate, Table: "collectionbox", HasWhere: false},
		},

		// --- Múltiples sentencias (stacking) ---
		// El parser cuenta las sentencias del árbol: más de una => rechazo.
		// Inmune a comentarios que intenten esconder el ';'.
		{
			name:    "dos sentencias con ; se rechazan",
			sql:     "UPDATE collectionbox SET amount = 0 WHERE id = 1; DELETE FROM collectionbox",
			wantErr: guard.ErrMultipleStatements,
		},
		{
			name:    "segunda sentencia tras un comentario se rechaza",
			sql:     "UPDATE collectionbox SET amount = 0 WHERE id = 1; -- inocente\nDROP TABLE collectionbox",
			wantErr: guard.ErrMultipleStatements,
		},

		// --- DDL / DROP / TRUNCATE / SELECT ---
		// No son UPDATE/DELETE/INSERT: el parser los marca como operación no
		// soportada (unsupportedOp) para que el checker los rechace.
		{
			name:     "DROP TABLE es operación no soportada",
			sql:      "DROP TABLE collectionbox",
			wantStmt: guard.Statement{Op: unsupportedOp},
		},
		{
			name:     "TRUNCATE es operación no soportada",
			sql:      "TRUNCATE TABLE collectionbox",
			wantStmt: guard.Statement{Op: unsupportedOp},
		},
		{
			name:     "ALTER TABLE es operación no soportada",
			sql:      "ALTER TABLE collectionbox ADD COLUMN x int",
			wantStmt: guard.Statement{Op: unsupportedOp},
		},
		{
			name:     "SELECT es operación no soportada",
			sql:      "SELECT * FROM collectionbox",
			wantStmt: guard.Statement{Op: unsupportedOp},
		},

		// --- INSERT: solo VALUES ---
		{
			name:     "INSERT ... VALUES es válido",
			sql:      "INSERT INTO collectionbox (amount) VALUES (10)",
			wantStmt: guard.Statement{Op: guard.OpInsert, Table: "collectionbox", InsertFromSelect: false},
		},
		{
			name:     "INSERT ... SELECT se marca como FromSelect",
			sql:      "INSERT INTO collectionbox (amount) SELECT amount FROM collectionbox",
			wantStmt: guard.Statement{Op: guard.OpInsert, Table: "collectionbox", InsertFromSelect: true},
		},

		// --- WHERE 1=1 / true (política: se permite; la red es el tope de filas) ---
		// El parser SÍ ve un WHERE (HasWhere=true). No intentamos detectar
		// tautologías por sintaxis: MAX_AFFECTED_ROWS es la salvaguarda real.
		{
			name:     "WHERE 1=1 es un WHERE válido a nivel parseo",
			sql:      "UPDATE collectionbox SET amount = 0 WHERE 1=1",
			wantStmt: guard.Statement{Op: guard.OpUpdate, Table: "collectionbox", HasWhere: true},
		},
		{
			name:       "WHERE true es un WHERE válido a nivel parseo (postgres)",
			sql:        "UPDATE collectionbox SET amount = 0 WHERE true",
			wantStmt:   guard.Statement{Op: guard.OpUpdate, Table: "collectionbox", HasWhere: true},
			onlyEngine: "postgres",
		},

		// --- Subqueries en WHERE (política: se permiten) ---
		// Es un WHERE legítimo; el tope de filas y el usuario restringido acotan el
		// daño. La subquery solo lee tablas que el usuario ya puede ver.
		{
			name:     "subquery en WHERE es un WHERE válido",
			sql:      "DELETE FROM collectionbox WHERE id IN (SELECT id FROM collectionbox WHERE amount = 0)",
			wantStmt: guard.Statement{Op: guard.OpDelete, Table: "collectionbox", HasWhere: true},
		},

		// --- CTE (WITH ... UPDATE) ---
		// La tabla objetivo del UPDATE/DELETE es la del propio UPDATE/DELETE, NO la
		// del CTE. El checker la valida contra la whitelist. El CTE auxiliar solo
		// lee. Este caso fija que la tabla objetivo se extrae correctamente y no la
		// del WITH.
		{
			name:     "CTE con UPDATE: la tabla objetivo es la del UPDATE",
			sql:      "WITH x AS (SELECT id FROM collectionbox WHERE amount = 0) UPDATE collectionbox SET amount = 1 WHERE id IN (SELECT id FROM x)",
			wantStmt: guard.Statement{Op: guard.OpUpdate, Table: "collectionbox", HasWhere: true},
		},
	}

	for _, engine := range bothParsers() {
		for _, tc := range cases {
			if tc.onlyEngine != "" && tc.onlyEngine != engine.name {
				continue
			}
			t.Run(engine.name+"/"+tc.name, func(t *testing.T) {
				got, err := engine.parse(tc.sql)
				if tc.wantErr != nil {
					if !errors.Is(err, tc.wantErr) {
						t.Fatalf("parse(%q) error = %v, want %v", tc.sql, err, tc.wantErr)
					}
					return
				}
				if err != nil {
					t.Fatalf("parse(%q) error inesperado = %v", tc.sql, err)
				}
				if got.Op != tc.wantStmt.Op {
					t.Errorf("Op = %q, want %q", got.Op, tc.wantStmt.Op)
				}
				if got.Table != tc.wantStmt.Table {
					t.Errorf("Table = %q, want %q", got.Table, tc.wantStmt.Table)
				}
				if got.HasWhere != tc.wantStmt.HasWhere {
					t.Errorf("HasWhere = %v, want %v", got.HasWhere, tc.wantStmt.HasWhere)
				}
				if got.InsertFromSelect != tc.wantStmt.InsertFromSelect {
					t.Errorf("InsertFromSelect = %v, want %v", got.InsertFromSelect, tc.wantStmt.InsertFromSelect)
				}
			})
		}
	}
}

// TestPostgresFoldsUnquotedIdentifiers fija una propiedad de seguridad concreta y
// asimétrica entre motores: Postgres normaliza los identificadores SIN comillas
// a minúscula, mientras que si van entre comillas dobles preserva el casing. La
// whitelist se compara de forma exacta y case-sensitive, así que quien la
// configura debe escribir el nombre de tabla tal como el motor lo normaliza
// (documentado en docs/RESTRICTED-USER.md). Este test lo demuestra para que no
// se rompa en silencio.
func TestPostgresFoldsUnquotedIdentifiers(t *testing.T) {
	// Sin comillas: Postgres foldea a minúscula.
	stmt, err := parsePostgres("UPDATE CollectionBox SET amount = 0 WHERE id = 1")
	if err != nil {
		t.Fatalf("parse sin comillas: %v", err)
	}
	if stmt.Table != "collectionbox" {
		t.Fatalf("Postgres sin comillas: Table = %q, want %q (folding a minúscula)", stmt.Table, "collectionbox")
	}

	// Con comillas dobles: Postgres preserva el casing exacto.
	stmt, err = parsePostgres(`UPDATE "CollectionBox" SET amount = 0 WHERE id = 1`)
	if err != nil {
		t.Fatalf("parse con comillas: %v", err)
	}
	if stmt.Table != "CollectionBox" {
		t.Fatalf("Postgres con comillas: Table = %q, want %q (casing preservado)", stmt.Table, "CollectionBox")
	}
}

// TestMySQLPreservesIdentifierCasing es la contraparte: MySQL/MariaDB (TiDB)
// preserva el casing del identificador tal como se escribe, sin foldear. Junto
// con el test de Postgres deja documentado el porqué de configurar la whitelist
// distinto según el motor.
func TestMySQLPreservesIdentifierCasing(t *testing.T) {
	stmt, err := parseMySQL("UPDATE CollectionBox SET amount = 0 WHERE id = 1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if stmt.Table != "CollectionBox" {
		t.Fatalf("MySQL: Table = %q, want %q (casing preservado)", stmt.Table, "CollectionBox")
	}
}

// TestCheckerOnParsed cierra el lazo: toma el Statement que produce el parser
// real y lo pasa por guard.Check con una whitelist realista, verificando el
// veredicto final (aceptar/rechazar) que ve el usuario. Es la prueba end-to-end
// de la guarda de sentencia, sin base de datos.
//
// Se usa la tabla en minúscula (collectionbox) para que el caso sea determinista
// en ambos motores (ver la nota de casing arriba).
func TestCheckerOnParsed(t *testing.T) {
	whitelist := []string{"collectionbox"}

	cases := []struct {
		name    string
		sql     string
		wantErr error // error final esperado de guard.Check (nil = aceptado)
	}{
		{"UPDATE sin WHERE se rechaza", "UPDATE collectionbox SET amount = 0", guard.ErrMissingWhere},
		{"DELETE sin WHERE se rechaza", "DELETE FROM collectionbox", guard.ErrMissingWhere},
		{"UPDATE con WHERE se acepta", "UPDATE collectionbox SET amount = 0 WHERE id = 1", nil},
		{"WHERE comentado se rechaza (no hay WHERE real)", "UPDATE collectionbox SET amount = 0 -- WHERE id = 1", guard.ErrMissingWhere},
		{"DROP se rechaza por operación", "DROP TABLE collectionbox", guard.ErrOperationNotAllowed},
		{"TRUNCATE se rechaza por operación", "TRUNCATE TABLE collectionbox", guard.ErrOperationNotAllowed},
		{"INSERT ... SELECT se rechaza", "INSERT INTO collectionbox (amount) SELECT amount FROM collectionbox", guard.ErrInsertFromSelect},
		{"tabla fuera de whitelist se rechaza", "DELETE FROM secrets WHERE id = 1", guard.ErrTableNotWhitelisted},
		{"WHERE 1=1 se acepta a nivel guarda (el tope de filas es la red)", "UPDATE collectionbox SET amount = 0 WHERE 1=1", nil},
		{"subquery en WHERE se acepta", "DELETE FROM collectionbox WHERE id IN (SELECT id FROM collectionbox WHERE amount = 0)", nil},
		{"CTE con UPDATE a tabla whitelisteada se acepta", "WITH x AS (SELECT id FROM collectionbox) UPDATE collectionbox SET amount = 1 WHERE id IN (SELECT id FROM x)", nil},
	}

	for _, engine := range bothParsers() {
		for _, tc := range cases {
			t.Run(engine.name+"/"+tc.name, func(t *testing.T) {
				stmt, err := engine.parse(tc.sql)
				if err != nil {
					// Un error de parseo también es un rechazo válido, salvo que
					// esperáramos aceptar la sentencia.
					if tc.wantErr == nil {
						t.Fatalf("parse(%q) error inesperado = %v", tc.sql, err)
					}
					return
				}
				gotErr := guard.Check(stmt, whitelist)
				if tc.wantErr == nil {
					if gotErr != nil {
						t.Fatalf("Check rechazó una sentencia legítima: %v (sql=%q)", gotErr, tc.sql)
					}
					return
				}
				if !errors.Is(gotErr, tc.wantErr) {
					t.Fatalf("Check(%q) = %v, want %v", tc.sql, gotErr, tc.wantErr)
				}
			})
		}
	}
}
