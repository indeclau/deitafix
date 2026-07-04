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
// dialecto, y verifica en un solo lugar tanto la clasificación del parser
// (guard.Statement) como el veredicto final del checker (guard.Check).
//
// Nota sobre el casing de la tabla: PostgreSQL "foldea" los identificadores sin
// comillas a minúscula (CollectionBox -> collectionbox); MySQL/MariaDB los
// preserva. Para que los casos genéricos sean deterministas en ambos motores se
// usa una tabla en minúscula (collectionbox), que matchea el folding de Postgres
// y una whitelist en minúscula. El folding en sí se fija aparte, en
// TestPostgresFoldsUnquotedIdentifiers, por ser una propiedad de seguridad
// relevante.

// whitelistForTests es la whitelist usada por el checker en estos tests.
var whitelistForTests = []string{"collectionbox"}

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

// securityCase describe una sentencia de entrada y todo lo que se espera de ella:
// la clasificación del parser y el veredicto del checker.
type securityCase struct {
	name string
	sql  string

	// wantParseErr, si no es nil, es el error centinela de parseo esperado (se
	// compara con errors.Is). Si es nil, se espera un parseo exitoso -> wantStmt.
	wantParseErr error

	// wantStmt es el Statement esperado cuando wantParseErr es nil.
	wantStmt guard.Statement

	// wantCheckErr es el veredicto esperado de guard.Check sobre el Statement:
	// nil = aceptado. Solo se evalúa cuando el parseo fue exitoso.
	wantCheckErr error

	// onlyEngine, si no está vacío, restringe el caso a un solo motor (SQL de un
	// dialecto puntual, p.ej. WHERE true de Postgres).
	onlyEngine string
}

// securityCases son los bypasses conocidos y las sentencias legítimas, con su
// comportamiento esperado de parseo y de checker, para ambos motores.
func securityCases() []securityCase {
	return []securityCase{
		// --- UPDATE/DELETE sin WHERE: el parser ve HasWhere=false; el checker rechaza. ---
		{
			name:         "UPDATE sin WHERE",
			sql:          "UPDATE collectionbox SET amount = 0",
			wantStmt:     guard.Statement{Op: guard.OpUpdate, Table: "collectionbox", HasWhere: false},
			wantCheckErr: guard.ErrMissingWhere,
		},
		{
			name:         "DELETE sin WHERE",
			sql:          "DELETE FROM collectionbox",
			wantStmt:     guard.Statement{Op: guard.OpDelete, Table: "collectionbox", HasWhere: false},
			wantCheckErr: guard.ErrMissingWhere,
		},

		// --- Comentarios que ocultan la intención: un WHERE comentado NO es un WHERE. ---
		{
			name:         "WHERE comentado con -- no cuenta como WHERE",
			sql:          "UPDATE collectionbox SET amount = 0 -- WHERE id = 1",
			wantStmt:     guard.Statement{Op: guard.OpUpdate, Table: "collectionbox", HasWhere: false},
			wantCheckErr: guard.ErrMissingWhere,
		},
		{
			name:         "WHERE comentado con /* */ no cuenta como WHERE",
			sql:          "DELETE FROM collectionbox /* WHERE id = 1 */",
			wantStmt:     guard.Statement{Op: guard.OpDelete, Table: "collectionbox", HasWhere: false},
			wantCheckErr: guard.ErrMissingWhere,
		},
		{
			name:         "comentario embebido no oculta la ausencia de WHERE",
			sql:          "UPDATE /* sneaky */ collectionbox SET amount = 0",
			wantStmt:     guard.Statement{Op: guard.OpUpdate, Table: "collectionbox", HasWhere: false},
			wantCheckErr: guard.ErrMissingWhere,
		},

		// --- Múltiples sentencias (stacking): el parser cuenta > 1 y rechaza. ---
		{
			name:         "dos sentencias con ; se rechazan",
			sql:          "UPDATE collectionbox SET amount = 0 WHERE id = 1; DELETE FROM collectionbox",
			wantParseErr: guard.ErrMultipleStatements,
		},
		{
			name:         "segunda sentencia tras un comentario se rechaza",
			sql:          "UPDATE collectionbox SET amount = 0 WHERE id = 1; -- inocente\nDROP TABLE collectionbox",
			wantParseErr: guard.ErrMultipleStatements,
		},

		// --- DDL / DROP / TRUNCATE / SELECT: operación no soportada. ---
		{
			name:         "DROP TABLE es operación no soportada",
			sql:          "DROP TABLE collectionbox",
			wantStmt:     guard.Statement{Op: unsupportedOp},
			wantCheckErr: guard.ErrOperationNotAllowed,
		},
		{
			name:         "TRUNCATE es operación no soportada",
			sql:          "TRUNCATE TABLE collectionbox",
			wantStmt:     guard.Statement{Op: unsupportedOp},
			wantCheckErr: guard.ErrOperationNotAllowed,
		},
		{
			name:         "ALTER TABLE es operación no soportada",
			sql:          "ALTER TABLE collectionbox ADD COLUMN x int",
			wantStmt:     guard.Statement{Op: unsupportedOp},
			wantCheckErr: guard.ErrOperationNotAllowed,
		},
		{
			name:         "SELECT es operación no soportada",
			sql:          "SELECT * FROM collectionbox",
			wantStmt:     guard.Statement{Op: unsupportedOp},
			wantCheckErr: guard.ErrOperationNotAllowed,
		},

		// --- INSERT: solo VALUES. ---
		{
			name:         "INSERT ... VALUES es válido",
			sql:          "INSERT INTO collectionbox (amount) VALUES (10)",
			wantStmt:     guard.Statement{Op: guard.OpInsert, Table: "collectionbox", InsertFromSelect: false},
			wantCheckErr: nil,
		},
		{
			name:         "INSERT ... SELECT se rechaza",
			sql:          "INSERT INTO collectionbox (amount) SELECT amount FROM collectionbox",
			wantStmt:     guard.Statement{Op: guard.OpInsert, Table: "collectionbox", InsertFromSelect: true},
			wantCheckErr: guard.ErrInsertFromSelect,
		},

		// --- WHERE 1=1 / true: política = permitido; la red es el tope de filas. ---
		{
			name:         "WHERE 1=1 es un WHERE válido y se acepta",
			sql:          "UPDATE collectionbox SET amount = 0 WHERE 1=1",
			wantStmt:     guard.Statement{Op: guard.OpUpdate, Table: "collectionbox", HasWhere: true},
			wantCheckErr: nil,
		},
		{
			name:         "WHERE true es un WHERE válido (postgres)",
			sql:          "UPDATE collectionbox SET amount = 0 WHERE true",
			wantStmt:     guard.Statement{Op: guard.OpUpdate, Table: "collectionbox", HasWhere: true},
			wantCheckErr: nil,
			onlyEngine:   "postgres",
		},

		// --- Subquery en WHERE: política = permitido. ---
		{
			name:         "subquery en WHERE es un WHERE válido y se acepta",
			sql:          "DELETE FROM collectionbox WHERE id IN (SELECT id FROM collectionbox WHERE amount = 0)",
			wantStmt:     guard.Statement{Op: guard.OpDelete, Table: "collectionbox", HasWhere: true},
			wantCheckErr: nil,
		},

		// --- CTE (WITH ... UPDATE): la tabla objetivo es la del UPDATE, no la del CTE. ---
		{
			name:         "CTE con UPDATE: la tabla objetivo es la del UPDATE y se acepta",
			sql:          "WITH x AS (SELECT id FROM collectionbox WHERE amount = 0) UPDATE collectionbox SET amount = 1 WHERE id IN (SELECT id FROM x)",
			wantStmt:     guard.Statement{Op: guard.OpUpdate, Table: "collectionbox", HasWhere: true},
			wantCheckErr: nil,
		},

		// --- Tabla fuera de whitelist: parsea bien, pero el checker la rechaza. ---
		{
			name:         "tabla fuera de whitelist se rechaza",
			sql:          "DELETE FROM secrets WHERE id = 1",
			wantStmt:     guard.Statement{Op: guard.OpDelete, Table: "secrets", HasWhere: true},
			wantCheckErr: guard.ErrTableNotWhitelisted,
		},
	}
}

// TestParserSecurity recorre los bypasses conocidos y sentencias legítimas y
// verifica, para cada parser real, tanto la clasificación (guard.Statement) como
// el veredicto final del checker (guard.Check). Es la prueba end-to-end de la
// guarda de sentencia, sin base de datos.
func TestParserSecurity(t *testing.T) {
	for _, engine := range bothParsers() {
		t.Run(engine.name, func(t *testing.T) {
			for _, tc := range securityCases() {
				if tc.onlyEngine != "" && tc.onlyEngine != engine.name {
					continue
				}
				t.Run(tc.name, func(t *testing.T) {
					verifySecurityCase(t, engine.parse, tc)
				})
			}
		})
	}
}

// verifySecurityCase corre parser + checker sobre un caso y verifica ambos
// resultados. Extraído para mantener baja la complejidad del cuerpo del test.
func verifySecurityCase(t *testing.T, parse func(string) (guard.Statement, error), tc securityCase) {
	t.Helper()

	stmt, err := parse(tc.sql)
	if tc.wantParseErr != nil {
		if !errors.Is(err, tc.wantParseErr) {
			t.Fatalf("parse(%q) error = %v, want %v", tc.sql, err, tc.wantParseErr)
		}
		return
	}
	if err != nil {
		t.Fatalf("parse(%q) error inesperado = %v", tc.sql, err)
	}

	assertStatement(t, stmt, tc.wantStmt)
	assertCheckVerdict(t, stmt, tc.wantCheckErr)
}

// assertStatement compara campo a campo el Statement parseado contra el esperado.
func assertStatement(t *testing.T, got, want guard.Statement) {
	t.Helper()
	if got.Op != want.Op {
		t.Errorf("Op = %q, want %q", got.Op, want.Op)
	}
	if got.Table != want.Table {
		t.Errorf("Table = %q, want %q", got.Table, want.Table)
	}
	if got.HasWhere != want.HasWhere {
		t.Errorf("HasWhere = %v, want %v", got.HasWhere, want.HasWhere)
	}
	if got.InsertFromSelect != want.InsertFromSelect {
		t.Errorf("InsertFromSelect = %v, want %v", got.InsertFromSelect, want.InsertFromSelect)
	}
}

// assertCheckVerdict verifica el veredicto de guard.Check sobre el Statement:
// wantErr nil = aceptado.
func assertCheckVerdict(t *testing.T, stmt guard.Statement, wantErr error) {
	t.Helper()
	gotErr := guard.Check(stmt, whitelistForTests)
	if wantErr == nil {
		if gotErr != nil {
			t.Fatalf("Check rechazó una sentencia legítima: %v (op=%q table=%q)", gotErr, stmt.Op, stmt.Table)
		}
		return
	}
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("Check(op=%q table=%q) = %v, want %v", stmt.Op, stmt.Table, gotErr, wantErr)
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

// TestMySQLTableResolution cubre la resolución de la tabla objetivo en el árbol
// de joins de TiDB (firstTableName / resolveTableName). Son casos de seguridad:
// en un UPDATE con JOIN la tabla objetivo es la de más a la izquierda, y una
// fuente que no es una tabla simple (subconsulta) devuelve "" para que el
// checker la rechace por whitelist.
func TestMySQLTableResolution(t *testing.T) {
	t.Run("UPDATE con JOIN: la tabla objetivo es la de más a la izquierda", func(t *testing.T) {
		stmt, err := parseMySQL("UPDATE t1 JOIN t2 ON t1.id = t2.id SET t1.x = 1 WHERE t1.id = 5")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if stmt.Table != "t1" {
			t.Fatalf("Table = %q, want t1 (la más a la izquierda del join)", stmt.Table)
		}
	})

	t.Run("DELETE con subconsulta como fuente devuelve tabla vacía", func(t *testing.T) {
		// DELETE con la tabla resuelta desde una derivada no es una tabla simple:
		// firstTableName devuelve "", y el checker lo rechaza por whitelist.
		stmt, err := parseMySQL("DELETE t FROM (SELECT id FROM collectionbox) AS t WHERE t.id = 1")
		if err != nil {
			// TiDB puede rechazar esta forma directamente; también es un rechazo válido.
			return
		}
		if stmt.Table != "" && stmt.Table != "t" {
			t.Fatalf("Table = %q; se esperaba vacío o el alias, nunca una tabla real whitelisteable", stmt.Table)
		}
	})
}
