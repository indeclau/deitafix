// Command deitafix es el entrypoint del servicio: carga la configuración,
// abre el motor con el usuario restringido y sirve la API preview → confirm.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/indeclau/deitafix/internal/ai"
	"github.com/indeclau/deitafix/internal/api"
	"github.com/indeclau/deitafix/internal/config"
	"github.com/indeclau/deitafix/internal/engine"
	"github.com/indeclau/deitafix/internal/store"
)

const (
	// tokenTTL es la ventana de validez de un token de preview.
	tokenTTL = 5 * time.Minute
	// gcInterval es cada cuánto se limpian tokens expirados.
	gcInterval = time.Minute
	// shutdownTimeout es el margen para cerrar conexiones al recibir la señal.
	shutdownTimeout = 10 * time.Second
	// schemaTTL es cuánto se cachea el esquema introspeccionado para NL → SQL.
	// El esquema de la whitelist cambia rara vez; un TTL amplio evita golpear
	// information_schema en cada sugerencia.
	schemaTTL = 5 * time.Minute
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("deitafix: %v", err)
	}
}

func run() error {
	// En desarrollo se puede usar un .env; en producción las variables las
	// provee el orquestador. Ignoramos el error: la ausencia de .env es normal.
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	eng, err := engine.Open(ctx, cfg.Engine, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = eng.Close() }()

	st := store.New(tokenTTL)
	go runGC(ctx, st)

	// Capa de IA: cliente real si hay AI_API_KEY, o un noop (disabled) que
	// degrada de forma limpia. La construcción decide cuál instanciar; el resto
	// del servicio funciona idéntico en ambos casos.
	aiClient := buildAIClient(cfg)
	schema := buildSchemaCache(eng, aiClient)

	svc := api.NewServiceWithAI(eng, st, cfg.TableWhitelist, int64(cfg.MaxAffectedRows), aiClient, schema)
	handler := api.NewRouter(svc, api.RouterConfig{
		Enabled:            cfg.Enabled,
		MCPEnabled:         cfg.MCPEnabled,
		MCPAuthToken:       cfg.MCPAuthToken,
		MCPPath:            cfg.MCPPath,
		MCPApprovalBaseURL: cfg.MCPApprovalBaseURL,
		UIAuthToken:        cfg.UIAuthToken,
	})

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("deitafix: motor=%s enabled=%t max_affected_rows=%d whitelist=%v mcp_enabled=%t mcp_path=%s ai_enabled=%t escuchando en %s",
		cfg.Engine, cfg.Enabled, cfg.MaxAffectedRows, cfg.TableWhitelist, cfg.MCPEnabled, cfg.MCPPath, cfg.AIEnabled(), srv.Addr)

	return serve(ctx, srv)
}

// buildAIClient instancia el cliente de IA según la config: el cliente real de
// Anthropic si hay AI_API_KEY, o el noop (disabled) que degrada de forma limpia.
// El resto del servicio funciona idéntico con cualquiera de los dos.
func buildAIClient(cfg config.Config) ai.Client {
	if !cfg.AIEnabled() {
		return ai.NewDisabled()
	}
	return ai.NewAnthropic(ai.Config{
		APIKey:  cfg.AIAPIKey,
		Model:   cfg.AIModel,
		BaseURL: cfg.AIBaseURL,
		Timeout: cfg.AITimeout,
	})
}

// buildSchemaCache arma el cache de introspección de esquema para NL → SQL, solo
// si la IA está habilitada y el motor soporta introspección. Si no, devuelve nil
// y el modelo trabaja solo con la intención (degradación limpia).
func buildSchemaCache(eng engine.Engine, aiClient ai.Client) *engine.SchemaCache {
	if !aiClient.Enabled() {
		return nil
	}
	in, ok := eng.(engine.Introspector)
	if !ok {
		return nil
	}
	return engine.NewSchemaCache(in, schemaTTL)
}

// serve arranca el servidor y hace shutdown graceful al cancelarse el contexto.
func serve(ctx context.Context, srv *http.Server) error {
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Println("deitafix: señal recibida, cerrando...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// runGC limpia tokens expirados periódicamente hasta que se cancela el contexto.
func runGC(ctx context.Context, st *store.Store) {
	ticker := time.NewTicker(gcInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st.GC()
		}
	}
}
