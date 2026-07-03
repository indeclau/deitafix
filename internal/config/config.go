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
)

// Defaults de configuración cuando la variable no está seteada.
const (
	defaultPort            = "8080"
	defaultMaxAffectedRows = 50
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
}

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

	return cfg, nil
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
