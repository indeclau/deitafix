// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

// Package config carga la configuración del servicio desde el entorno.
//
// Todas las opciones se leen de variables de entorno para que el binario sea
// stateless y desplegable en contenedores sin archivos de configuración. En
// desarrollo se puede usar un archivo .env (cargado con godotenv desde el
// entrypoint); en producción las variables las provee el orquestador.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults de configuración cuando la variable no está seteada.
const (
	defaultPort            = "8080"
	defaultMaxAffectedRows = 50
	defaultMCPPath         = "/mcp"
	defaultAITimeout       = 15 * time.Second
)

// Config es la configuración efectiva del servicio, ya validada.
type Config struct {
	// DatabaseURL es la conexión con el usuario restringido de la base.
	// Nunca debe ser el usuario de la aplicación.
	DatabaseURL string

	// Engine identifica el motor de la base apuntada por DatabaseURL.
	// Valores válidos: "postgres" | "mysql".
	Engine string

	// Enabled es el feature flag maestro (DATAFIX_ENABLED). Si es false, el
	// servicio arranca pero responde 503 a las rutas de escritura.
	Enabled bool

	// MaxAffectedRows es el tope de filas que una sentencia puede afectar.
	// Si el preview mide más filas, la operación se aborta.
	MaxAffectedRows int

	// TableWhitelist son las tablas que el servicio permite tocar, además de
	// la contención a nivel motor. Comparación exacta y sensible a mayúsculas
	// (Postgres distingue "CollectionBox" de collectionbox).
	TableWhitelist []string

	// Port es el puerto HTTP donde escucha el servicio.
	Port string

	// MCPEnabled es el on/off de la capa MCP (MCP_ENABLED). Si es false, el
	// endpoint MCP no se registra y el resto del servicio queda intacto.
	MCPEnabled bool

	// MCPAuthToken es el bearer que protege el endpoint MCP (MCP_AUTH_TOKEN).
	// Obligatorio si MCPEnabled es true: sin él, el arranque aborta.
	MCPAuthToken string

	// MCPPath es la ruta donde se monta el endpoint MCP (MCP_PATH, default /mcp).
	MCPPath string

	// MCPApprovalBaseURL es la base pública (esquema + host + puerto) desde donde
	// un humano aprueba, para armar la approval_url que devuelve la herramienta
	// MCP confirm (MCP_APPROVAL_BASE_URL, p. ej. "https://deitafix.midominio.com").
	// Si queda vacía, la approval_url es una ruta relativa (/pending).
	MCPApprovalBaseURL string

	// UIAuthToken protege OPCIONALMENTE la superficie humana (UI + confirm +
	// aprobaciones), para que la credencial MCP no la alcance (UI_AUTH_TOKEN).
	// La garantía dura de human-in-the-loop no depende de este token —se apoya
	// en el origen del token—; es una capa extra de defensa en profundidad. Si
	// está vacío, la superficie humana no exige bearer (comportamiento v0.1–v0.3).
	UIAuthToken string

	// AIAPIKey habilita OPCIONALMENTE la capa de IA (AI_API_KEY). Si está vacía,
	// la IA degrada de forma limpia: el resto del servicio funciona idéntico y la
	// capa de IA simplemente no se ofrece. La IA solo propone; nunca ejecuta.
	AIAPIKey string

	// AIModel es el modelo a usar (AI_MODEL). Si está vacío, se usa el default
	// documentado en el paquete ai (constante DefaultModel).
	AIModel string

	// AIBaseURL overridea OPCIONALMENTE el endpoint del proveedor (AI_BASE_URL).
	// Si está vacío, se usa el default de Anthropic.
	AIBaseURL string

	// AITimeout es el timeout por request de IA (AI_TIMEOUT), independiente del
	// timeout de la base. Default ~15s.
	AITimeout time.Duration
}

// AIEnabled indica si la capa de IA está configurada (hay AI_API_KEY). El
// entrypoint lo usa para decidir qué cliente instanciar (real vs disabled).
func (c Config) AIEnabled() bool { return c.AIAPIKey != "" }

// Load construye la Config a partir del entorno y la valida.
//
// Devuelve error si falta configuración obligatoria (DATABASE_URL, engine) o
// si algún valor numérico es inválido, de modo que el binario falle rápido al
// arrancar en lugar de comportarse de forma inesperada en runtime.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		Engine:          strings.ToLower(strings.TrimSpace(os.Getenv("DEITAFIX_ENGINE"))),
		Enabled:         parseBool(os.Getenv("DATAFIX_ENABLED")),
		MaxAffectedRows: defaultMaxAffectedRows,
		Port:            defaultPort,
	}

	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("config: DATABASE_URL es obligatoria")
	}

	if cfg.Engine == "" {
		// Si no se especifica, se infiere del esquema de la URL.
		cfg.Engine = inferEngine(cfg.DatabaseURL)
	}
	if cfg.Engine != "postgres" && cfg.Engine != "mysql" {
		return Config{}, fmt.Errorf("config: DEITAFIX_ENGINE inválido %q (esperado postgres|mysql)", cfg.Engine)
	}

	if v := os.Getenv("MAX_AFFECTED_ROWS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: MAX_AFFECTED_ROWS inválido %q: %w", v, err)
		}
		if n <= 0 {
			return Config{}, fmt.Errorf("config: MAX_AFFECTED_ROWS debe ser positivo, got %d", n)
		}
		cfg.MaxAffectedRows = n
	}

	cfg.TableWhitelist = parseList(os.Getenv("TABLE_WHITELIST"))

	if p := strings.TrimSpace(os.Getenv("PORT")); p != "" {
		cfg.Port = p
	}

	if err := loadMCP(&cfg); err != nil {
		return Config{}, err
	}

	cfg.UIAuthToken = strings.TrimSpace(os.Getenv("UI_AUTH_TOKEN"))

	if err := loadAI(&cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// loadAI lee y valida la configuración de la capa de IA.
//
// Degradación limpia, consistente con DATAFIX_ENABLED / MCP_ENABLED: si no hay
// AI_API_KEY, la capa queda apagada y el resto del servicio intacto (sin
// endpoints IA rotos, sin logs en loop). A diferencia de MCP, la ausencia de
// clave NO es un error: es el modo degradado esperado. Solo se falla el arranque
// si un valor presente es inválido (AI_TIMEOUT no parseable), para no arrancar
// con una config silenciosamente rota.
func loadAI(cfg *Config) error {
	cfg.AIAPIKey = strings.TrimSpace(os.Getenv("AI_API_KEY"))
	cfg.AIModel = strings.TrimSpace(os.Getenv("AI_MODEL"))
	cfg.AIBaseURL = strings.TrimRight(strings.TrimSpace(os.Getenv("AI_BASE_URL")), "/")

	cfg.AITimeout = defaultAITimeout
	if v := strings.TrimSpace(os.Getenv("AI_TIMEOUT")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("config: AI_TIMEOUT inválido %q: %w", v, err)
		}
		if d <= 0 {
			return fmt.Errorf("config: AI_TIMEOUT debe ser positivo, got %s", d)
		}
		cfg.AITimeout = d
	}

	return nil
}

// loadMCP lee y valida la configuración de la capa MCP.
//
// Degradación limpia, consistente con DATAFIX_ENABLED / AI_API_KEY: si
// MCP_ENABLED es false, la capa queda apagada y el resto del servicio intacto.
// Pero si está habilitada SIN token, se aborta el arranque con un error claro:
// un endpoint MCP sin auth sería un agujero, así que se falla rápido en vez de
// exponerlo.
func loadMCP(cfg *Config) error {
	cfg.MCPEnabled = parseBool(os.Getenv("MCP_ENABLED"))
	cfg.MCPAuthToken = strings.TrimSpace(os.Getenv("MCP_AUTH_TOKEN"))

	cfg.MCPPath = strings.TrimSpace(os.Getenv("MCP_PATH"))
	if cfg.MCPPath == "" {
		cfg.MCPPath = defaultMCPPath
	}
	if !strings.HasPrefix(cfg.MCPPath, "/") {
		return fmt.Errorf("config: MCP_PATH debe empezar con '/', got %q", cfg.MCPPath)
	}

	// Base URL para la approval_url; se normaliza sin '/' final para concatenar
	// limpio con la ruta de aprobación.
	cfg.MCPApprovalBaseURL = strings.TrimRight(strings.TrimSpace(os.Getenv("MCP_APPROVAL_BASE_URL")), "/")

	if cfg.MCPEnabled && cfg.MCPAuthToken == "" {
		return errors.New("config: MCP_ENABLED=true requiere MCP_AUTH_TOKEN (no se expone el endpoint MCP sin auth)")
	}

	return nil
}

// inferEngine deduce el motor a partir del esquema de la connection string.
func inferEngine(url string) string {
	switch {
	case strings.HasPrefix(url, "postgres://"), strings.HasPrefix(url, "postgresql://"):
		return "postgres"
	case strings.HasPrefix(url, "mysql://"):
		return "mysql"
	default:
		return ""
	}
}

// parseBool interpreta el feature flag de forma tolerante. Solo los valores
// afirmativos explícitos habilitan el servicio; cualquier otra cosa (incluida
// la ausencia) lo deja apagado, que es el default seguro.
func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// parseList parsea una lista separada por comas, ignorando espacios y vacíos.
func parseList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
