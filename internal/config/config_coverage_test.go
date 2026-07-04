// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

package config

import (
	"strings"
	"testing"
)

// TestInferEngine cubre la deducción del motor a partir del esquema de la URL,
// incluido el caso de esquema desconocido (que deja el motor vacío para que Load
// lo rechace).
func TestInferEngine(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"postgres://u:p@h:5432/db", "postgres"},
		{"postgresql://u:p@h:5432/db", "postgres"},
		{"mysql://u:p@h:3306/db", "mysql"},
		{"mariadb://u:p@h:3306/db", ""}, // esquema no reconocido
		{"http://nope", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := inferEngine(tc.url); got != tc.want {
			t.Errorf("inferEngine(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// TestParseList cubre el parseo de la whitelist: vacíos, espacios, y elementos
// que quedan tras el trim.
func TestParseList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"a", []string{"a"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b ,, c ", []string{"a", "b", "c"}}, // espacios y vacío intermedio
		{",,,", nil},                              // solo separadores
	}
	for _, tc := range cases {
		got := parseList(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("parseList(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseList(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

// TestParseBool cubre el feature flag tolerante: solo afirmativos explícitos
// habilitan; todo lo demás (default seguro) queda en false.
func TestParseBool(t *testing.T) {
	truthy := []string{"1", "true", "TRUE", "Yes", "on", " on "}
	for _, v := range truthy {
		if !parseBool(v) {
			t.Errorf("parseBool(%q) = false, want true", v)
		}
	}
	falsy := []string{"", "0", "false", "no", "off", "nope", "2"}
	for _, v := range falsy {
		if parseBool(v) {
			t.Errorf("parseBool(%q) = true, want false", v)
		}
	}
}

// TestLoadInfersEngineFromURL cubre la rama de Load donde DEITAFIX_ENGINE no se
// setea y el motor se infiere del esquema de DATABASE_URL.
func TestLoadInfersEngineFromURL(t *testing.T) {
	setEnv(t, map[string]string{
		"DEITAFIX_ENGINE": "", // forzar la inferencia
		"DATABASE_URL":    "mysql://u:p@localhost:3306/db",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Engine != "mysql" {
		t.Fatalf("Engine inferido = %q, want mysql", cfg.Engine)
	}
}

// TestLoadRejectsMissingDatabaseURL cubre la rama de error de DATABASE_URL vacía.
func TestLoadRejectsMissingDatabaseURL(t *testing.T) {
	setEnv(t, map[string]string{"DATABASE_URL": ""})
	if _, err := Load(); err == nil {
		t.Fatal("Load sin DATABASE_URL debía fallar")
	}
}

// TestLoadRejectsUnknownEngine cubre la rama de error de un motor no soportado
// (esquema de URL desconocido y sin DEITAFIX_ENGINE que lo salve).
func TestLoadRejectsUnknownEngine(t *testing.T) {
	setEnv(t, map[string]string{
		"DEITAFIX_ENGINE": "",
		"DATABASE_URL":    "oracle://u:p@localhost/db",
	})
	_, err := Load()
	if err == nil {
		t.Fatal("Load con motor no inferible debía fallar")
	}
	if !strings.Contains(err.Error(), "DEITAFIX_ENGINE") {
		t.Fatalf("error = %v, want mención a DEITAFIX_ENGINE", err)
	}
}

// TestLoadMaxAffectedRows cubre las ramas de MAX_AFFECTED_ROWS: valor válido,
// no numérico, y no positivo.
func TestLoadMaxAffectedRows(t *testing.T) {
	t.Run("valor válido lo setea", func(t *testing.T) {
		setEnv(t, map[string]string{"MAX_AFFECTED_ROWS": "123"})
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.MaxAffectedRows != 123 {
			t.Fatalf("MaxAffectedRows = %d, want 123", cfg.MaxAffectedRows)
		}
	})

	t.Run("no numérico aborta", func(t *testing.T) {
		setEnv(t, map[string]string{"MAX_AFFECTED_ROWS": "muchas"})
		if _, err := Load(); err == nil {
			t.Fatal("MAX_AFFECTED_ROWS no numérico debía fallar")
		}
	})

	t.Run("no positivo aborta", func(t *testing.T) {
		setEnv(t, map[string]string{"MAX_AFFECTED_ROWS": "0"})
		if _, err := Load(); err == nil {
			t.Fatal("MAX_AFFECTED_ROWS = 0 debía fallar")
		}
	})
}

// TestLoadCustomPortAndWhitelist cubre las ramas de PORT y TABLE_WHITELIST.
func TestLoadCustomPortAndWhitelist(t *testing.T) {
	setEnv(t, map[string]string{
		"PORT":            "9090",
		"TABLE_WHITELIST": "CollectionBox, AuditLog",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != "9090" {
		t.Fatalf("Port = %q, want 9090", cfg.Port)
	}
	if len(cfg.TableWhitelist) != 2 || cfg.TableWhitelist[0] != "CollectionBox" || cfg.TableWhitelist[1] != "AuditLog" {
		t.Fatalf("TableWhitelist = %v, want [CollectionBox AuditLog]", cfg.TableWhitelist)
	}
}
