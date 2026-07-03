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

	svc := api.NewService(eng, st, cfg.TableWhitelist, int64(cfg.MaxAffectedRows))
	handler := api.NewRouter(svc, cfg.Enabled)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("deitafix: motor=%s enabled=%t max_affected_rows=%d whitelist=%v escuchando en %s",
		cfg.Engine, cfg.Enabled, cfg.MaxAffectedRows, cfg.TableWhitelist, srv.Addr)

	return serve(ctx, srv)
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
