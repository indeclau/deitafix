// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

package engine

import (
	"strings"
	"testing"
)

// TestNormalizeMySQLDSN cubre la conversión de URL mysql:// al DSN del driver y
// el passthrough de un DSN nativo, verificando que en ambos casos se fuerza
// parseTime y que los componentes se trasladan bien.
func TestNormalizeMySQLDSN(t *testing.T) {
	t.Run("URL mysql:// se convierte a DSN tcp", func(t *testing.T) {
		got, err := normalizeMySQLDSN("mysql://user:pass@localhost:3306/mydb")
		if err != nil {
			t.Fatalf("normalizeMySQLDSN: %v", err)
		}
		// Forma esperada: user:pass@tcp(localhost:3306)/mydb?parseTime=true
		for _, want := range []string{"user:pass@tcp(localhost:3306)/mydb", "parseTime=true"} {
			if !strings.Contains(got, want) {
				t.Errorf("DSN %q no contiene %q", got, want)
			}
		}
	})

	t.Run("URL sin password", func(t *testing.T) {
		got, err := normalizeMySQLDSN("mysql://user@localhost:3306/mydb")
		if err != nil {
			t.Fatalf("normalizeMySQLDSN: %v", err)
		}
		if !strings.Contains(got, "user@tcp(localhost:3306)/mydb") {
			t.Errorf("DSN %q no tiene el user sin password esperado", got)
		}
	})

	t.Run("DSN nativo se respeta y fuerza parseTime", func(t *testing.T) {
		got, err := normalizeMySQLDSN("user:pass@tcp(db:3306)/mydb")
		if err != nil {
			t.Fatalf("normalizeMySQLDSN: %v", err)
		}
		if !strings.Contains(got, "parseTime=true") {
			t.Errorf("DSN nativo %q debía forzar parseTime=true", got)
		}
		if !strings.Contains(got, "tcp(db:3306)/mydb") {
			t.Errorf("DSN nativo %q perdió el host/db", got)
		}
	})

	t.Run("DSN nativo inválido devuelve error", func(t *testing.T) {
		if _, err := normalizeMySQLDSN("esto-no-es-un-dsn-valido@@@"); err == nil {
			t.Fatal("un DSN nativo inválido debía devolver error")
		}
	})
}
