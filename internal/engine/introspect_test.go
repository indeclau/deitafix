// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeIntrospector cuenta las llamadas y devuelve un resultado o error fijo, para
// verificar el comportamiento de cache sin una base real.
type fakeIntrospector struct {
	calls int
	cols  map[string][]ColumnInfo
	err   error
}

func (f *fakeIntrospector) Columns(_ context.Context, _ []string) (map[string][]ColumnInfo, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.cols, nil
}

func TestSchemaCache(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	newCache := func(in Introspector, ttl time.Duration) (*SchemaCache, *time.Time) {
		clock := base
		c := NewSchemaCache(in, ttl)
		c.now = func() time.Time { return clock }
		return c, &clock
	}

	t.Run("cachea y sirve del cache mientras está fresco", func(t *testing.T) {
		in := &fakeIntrospector{cols: map[string][]ColumnInfo{"t": {{Name: "id", Type: "int"}}}}
		c, _ := newCache(in, time.Minute)

		if _, err := c.Columns(ctx, []string{"t"}); err != nil {
			t.Fatalf("primera llamada: %v", err)
		}
		if _, err := c.Columns(ctx, []string{"t"}); err != nil {
			t.Fatalf("segunda llamada: %v", err)
		}
		if in.calls != 1 {
			t.Fatalf("introspector llamado %d veces, want 1 (la 2ª sale del cache)", in.calls)
		}
	})

	t.Run("reintrospecciona tras expirar el TTL", func(t *testing.T) {
		in := &fakeIntrospector{cols: map[string][]ColumnInfo{"t": {{Name: "id", Type: "int"}}}}
		c, clock := newCache(in, time.Minute)

		if _, err := c.Columns(ctx, []string{"t"}); err != nil {
			t.Fatalf("primera llamada: %v", err)
		}
		*clock = clock.Add(2 * time.Minute) // más allá del TTL
		if _, err := c.Columns(ctx, []string{"t"}); err != nil {
			t.Fatalf("tras expirar: %v", err)
		}
		if in.calls != 2 {
			t.Fatalf("introspector llamado %d veces, want 2 (cache expirado)", in.calls)
		}
	})

	t.Run("reintrospecciona si cambia el set de tablas", func(t *testing.T) {
		in := &fakeIntrospector{cols: map[string][]ColumnInfo{}}
		c, _ := newCache(in, time.Minute)

		_, _ = c.Columns(ctx, []string{"a"})
		_, _ = c.Columns(ctx, []string{"a", "b"}) // set distinto: no reusa el cache
		if in.calls != 2 {
			t.Fatalf("introspector llamado %d veces, want 2 (cambió el set)", in.calls)
		}
	})

	t.Run("un error no se cachea (se reintenta)", func(t *testing.T) {
		in := &fakeIntrospector{err: errors.New("boom")}
		c, _ := newCache(in, time.Minute)

		if _, err := c.Columns(ctx, []string{"t"}); err == nil {
			t.Fatal("se esperaba error")
		}
		if _, err := c.Columns(ctx, []string{"t"}); err == nil {
			t.Fatal("se esperaba error en el reintento")
		}
		if in.calls != 2 {
			t.Fatalf("introspector llamado %d veces, want 2 (el error no se cachea)", in.calls)
		}
	})
}
