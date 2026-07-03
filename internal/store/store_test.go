package store

import (
	"errors"
	"testing"
	"time"
)

// newTestStore crea un Store con reloj controlable para tests deterministas.
func newTestStore(ttl time.Duration) (*Store, *time.Time) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := base
	s := New(ttl)
	s.now = func() time.Time { return clock }
	return s, &clock
}

func TestPutTakeRoundTrip(t *testing.T) {
	s, _ := newTestStore(time.Minute)

	token, err := s.Put(Entry{SQL: "UPDATE x SET a=1 WHERE id=1", Table: "x", AffectedRows: 1})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if token == "" {
		t.Fatal("Put devolvió token vacío")
	}

	got, err := s.Take(token)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got.SQL != "UPDATE x SET a=1 WHERE id=1" || got.AffectedRows != 1 {
		t.Fatalf("Entry recuperada inesperada: %+v", got)
	}
}

func TestTakeIsSingleUse(t *testing.T) {
	s, _ := newTestStore(time.Minute)
	token, _ := s.Put(Entry{SQL: "DELETE FROM x WHERE id=1"})

	if _, err := s.Take(token); err != nil {
		t.Fatalf("primer Take falló: %v", err)
	}
	if _, err := s.Take(token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("segundo Take debía dar ErrNotFound, got %v", err)
	}
}

func TestTakeUnknownToken(t *testing.T) {
	s, _ := newTestStore(time.Minute)
	if _, err := s.Take("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("token desconocido debía dar ErrNotFound, got %v", err)
	}
}

func TestTakeExpiredToken(t *testing.T) {
	s, clock := newTestStore(time.Minute)
	token, _ := s.Put(Entry{SQL: "UPDATE x SET a=1 WHERE id=1"})

	// Avanzar el reloj más allá del TTL.
	*clock = clock.Add(2 * time.Minute)

	if _, err := s.Take(token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("token expirado debía dar ErrNotFound, got %v", err)
	}
}

func TestGCRemovesExpired(t *testing.T) {
	s, clock := newTestStore(time.Minute)
	_, _ = s.Put(Entry{SQL: "a"})
	_, _ = s.Put(Entry{SQL: "b"})
	if s.Len() != 2 {
		t.Fatalf("esperaba 2 items, got %d", s.Len())
	}

	*clock = clock.Add(2 * time.Minute)
	s.GC()

	if s.Len() != 0 {
		t.Fatalf("GC debía dejar 0 items, got %d", s.Len())
	}
}

func TestTokensAreUnique(t *testing.T) {
	s, _ := newTestStore(time.Minute)
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		token, err := s.Put(Entry{SQL: "x"})
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if seen[token] {
			t.Fatalf("token duplicado: %s", token)
		}
		seen[token] = true
	}
}
