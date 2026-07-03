package config

import (
	"strings"
	"testing"
	"time"
)

// setEnv setea las variables mínimas para un Load válido más las extra dadas,
// limpiando todo al terminar el test.
func setEnv(t *testing.T, extra map[string]string) {
	t.Helper()
	base := map[string]string{
		"DATABASE_URL":    "postgres://prod_datafix:pw@localhost:5432/db",
		"DEITAFIX_ENGINE": "postgres",
		"DATAFIX_ENABLED": "true",
	}
	for k, v := range extra {
		base[k] = v
	}
	// Limpiar todas las claves relevantes primero para no heredar del entorno.
	for _, k := range []string{
		"DATABASE_URL", "DEITAFIX_ENGINE", "DATAFIX_ENABLED", "MAX_AFFECTED_ROWS",
		"TABLE_WHITELIST", "PORT", "MCP_ENABLED", "MCP_AUTH_TOKEN", "MCP_PATH",
		"MCP_APPROVAL_BASE_URL", "UI_AUTH_TOKEN",
		"AI_API_KEY", "AI_MODEL", "AI_BASE_URL", "AI_TIMEOUT",
	} {
		t.Setenv(k, "")
	}
	for k, v := range base {
		t.Setenv(k, v)
	}
}

func TestMCPDisabledByDefault(t *testing.T) {
	setEnv(t, nil)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MCPEnabled {
		t.Fatal("MCP debía estar deshabilitado por default")
	}
	if cfg.MCPPath != "/mcp" {
		t.Fatalf("MCPPath default = %q, want /mcp", cfg.MCPPath)
	}
}

func TestMCPEnabledRequiresToken(t *testing.T) {
	setEnv(t, map[string]string{"MCP_ENABLED": "true"})
	_, err := Load()
	if err == nil {
		t.Fatal("MCP_ENABLED=true sin MCP_AUTH_TOKEN debía abortar el arranque")
	}
	if !strings.Contains(err.Error(), "MCP_AUTH_TOKEN") {
		t.Fatalf("error = %v, want mención a MCP_AUTH_TOKEN", err)
	}
}

func TestMCPEnabledWithToken(t *testing.T) {
	setEnv(t, map[string]string{
		"MCP_ENABLED":           "true",
		"MCP_AUTH_TOKEN":        "secret",
		"MCP_APPROVAL_BASE_URL": "https://host:8080/",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.MCPEnabled || cfg.MCPAuthToken != "secret" {
		t.Fatalf("config MCP inesperada: %+v", cfg)
	}
	// La base URL se normaliza sin '/' final.
	if cfg.MCPApprovalBaseURL != "https://host:8080" {
		t.Fatalf("MCPApprovalBaseURL = %q, want sin barra final", cfg.MCPApprovalBaseURL)
	}
}

func TestMCPPathMustBeAbsolute(t *testing.T) {
	setEnv(t, map[string]string{
		"MCP_ENABLED":    "true",
		"MCP_AUTH_TOKEN": "secret",
		"MCP_PATH":       "mcp",
	})
	if _, err := Load(); err == nil {
		t.Fatal("MCP_PATH sin '/' inicial debía fallar")
	}
}

func TestUIAuthTokenOptional(t *testing.T) {
	setEnv(t, nil)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UIAuthToken != "" {
		t.Fatalf("UIAuthToken debía ser vacío por default, got %q", cfg.UIAuthToken)
	}

	setEnv(t, map[string]string{"UI_AUTH_TOKEN": "ui-secret"})
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UIAuthToken != "ui-secret" {
		t.Fatalf("UIAuthToken = %q, want ui-secret", cfg.UIAuthToken)
	}
}

// TestAIDisabledByDefault verifica la degradación limpia: sin AI_API_KEY, la
// capa de IA está apagada y el arranque NO falla (a diferencia de MCP, donde
// habilitar sin token sí aborta). AITimeout toma su default.
func TestAIDisabledByDefault(t *testing.T) {
	setEnv(t, nil)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AIEnabled() {
		t.Fatal("la IA debía estar deshabilitada por default (sin AI_API_KEY)")
	}
	if cfg.AITimeout != defaultAITimeout {
		t.Fatalf("AITimeout default = %v, want %v", cfg.AITimeout, defaultAITimeout)
	}
}

func TestAIEnabledWithKey(t *testing.T) {
	setEnv(t, map[string]string{
		"AI_API_KEY":  "sk-test",
		"AI_MODEL":    "modelo-x",
		"AI_BASE_URL": "https://proxy.local/",
		"AI_TIMEOUT":  "30s",
	})
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.AIEnabled() {
		t.Fatal("la IA debía estar habilitada con AI_API_KEY")
	}
	if cfg.AIModel != "modelo-x" {
		t.Fatalf("AIModel = %q, want modelo-x", cfg.AIModel)
	}
	// La base URL se normaliza sin '/' final.
	if cfg.AIBaseURL != "https://proxy.local" {
		t.Fatalf("AIBaseURL = %q, want sin barra final", cfg.AIBaseURL)
	}
	if cfg.AITimeout != 30*time.Second {
		t.Fatalf("AITimeout = %v, want 30s", cfg.AITimeout)
	}
}

func TestAITimeoutInvalidAborts(t *testing.T) {
	setEnv(t, map[string]string{"AI_TIMEOUT": "no-es-una-duracion"})
	if _, err := Load(); err == nil {
		t.Fatal("AI_TIMEOUT inválido debía abortar el arranque")
	}
}
