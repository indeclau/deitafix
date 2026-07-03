// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

package engine

import (
	"context"
	"fmt"
)

// Asserts de compile-time: ambos motores implementan Engine y, además, la
// capacidad opcional Introspector (que consume la capa de IA para NL → SQL).
var (
	_ Engine       = (*Postgres)(nil)
	_ Engine       = (*MySQL)(nil)
	_ Introspector = (*Postgres)(nil)
	_ Introspector = (*MySQL)(nil)
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
