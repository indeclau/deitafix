package engine

import (
	"context"
	"fmt"
)

// Asserts de compile-time: ambos motores implementan Engine.
var (
	_ Engine = (*Postgres)(nil)
	_ Engine = (*MySQL)(nil)
)

// Open crea el Engine correspondiente al motor indicado, conectándose a la URL
// dada. Es el punto de entrada que usan el entrypoint y los tests.
func Open(ctx context.Context, engineName, url string) (Engine, error) {
	switch engineName {
	case "postgres":
		return NewPostgres(ctx, url)
	case "mysql":
		return NewMySQL(ctx, url)
	default:
		return nil, fmt.Errorf("engine: motor no soportado %q", engineName)
	}
}
