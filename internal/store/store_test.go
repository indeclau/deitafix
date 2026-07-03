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

// --- Origin y máquina de estados (flujo MCP) ---

func TestPutDefaultsToUIOrigin(t *testing.T) {
	s, _ := newTestStore(time.Minute)
	token, _ := s.Put(Entry{SQL: "UPDATE x SET a=1 WHERE id=1"})
	got, err := s.Take(token)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got.Origin != OriginUI {
		t.Fatalf("Put debía marcar origin=ui, got %q", got.Origin)
	}
}

func TestMCPApprovalFlow(t *testing.T) {
	s, _ := newTestStore(time.Minute)
	token, _ := s.PutWithOrigin(Entry{
		SQL: "UPDATE x SET a=1 WHERE id=1", Op: "UPDATE", Table: "x", AffectedRows: 1,
	}, OriginMCP)

	// Aún no está pendiente: no aparece en la lista ni es aprobable.
	if len(s.ListPending()) != 0 {
		t.Fatal("un token en previewed no debía estar en la lista de pendientes")
	}
	if _, err := s.Approve(token); !errors.Is(err, ErrWrongState) {
		t.Fatalf("Approve en previewed = %v, want ErrWrongState", err)
	}

	// Solicitar aprobación: pasa a pending_approval sin consumirse.
	if _, err := s.RequestApproval(token); err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	pend := s.ListPending()
	if len(pend) != 1 || pend[0].Token != token {
		t.Fatalf("ListPending inesperado: %+v", pend)
	}
	if pend[0].Op != "UPDATE" || pend[0].Table != "x" || pend[0].AffectedRows != 1 {
		t.Fatalf("datos del pendiente inesperados: %+v", pend[0])
	}

	// Peek no consume.
	if _, err := s.Peek(token); err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if len(s.ListPending()) != 1 {
		t.Fatal("Peek no debía consumir el token")
	}

	// Approve consume y devuelve la entry.
	entry, err := s.Approve(token)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if entry.SQL != "UPDATE x SET a=1 WHERE id=1" {
		t.Fatalf("entry aprobada inesperada: %+v", entry)
	}
	// Consumido: no reaprobable ni listado.
	if _, err := s.Approve(token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("segundo Approve = %v, want ErrNotFound", err)
	}
	if len(s.ListPending()) != 0 {
		t.Fatal("el token aprobado no debía seguir listado")
	}
}

func TestRequestApprovalTwiceFails(t *testing.T) {
	s, _ := newTestStore(time.Minute)
	token, _ := s.PutWithOrigin(Entry{SQL: "x"}, OriginMCP)
	if _, err := s.RequestApproval(token); err != nil {
		t.Fatalf("primer RequestApproval: %v", err)
	}
	if _, err := s.RequestApproval(token); !errors.Is(err, ErrWrongState) {
		t.Fatalf("segundo RequestApproval = %v, want ErrWrongState", err)
	}
}

func TestRejectConsumesPending(t *testing.T) {
	s, _ := newTestStore(time.Minute)
	token, _ := s.PutWithOrigin(Entry{SQL: "x"}, OriginMCP)
	_, _ = s.RequestApproval(token)

	if _, err := s.Reject(token); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if _, err := s.Approve(token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Approve tras Reject = %v, want ErrNotFound", err)
	}
}

func TestExpiredTokenNotApprovable(t *testing.T) {
	s, clock := newTestStore(time.Minute)
	token, _ := s.PutWithOrigin(Entry{SQL: "x"}, OriginMCP)
	_, _ = s.RequestApproval(token)

	*clock = clock.Add(2 * time.Minute)

	if _, err := s.Approve(token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Approve de token expirado = %v, want ErrNotFound", err)
	}
	if len(s.ListPending()) != 0 {
		t.Fatal("un token expirado no debía aparecer en la lista")
	}
}

func TestListPendingOnlyMCP(t *testing.T) {
	s, _ := newTestStore(time.Minute)
	// Un token UI (no debe listarse aunque exista) y uno MCP pendiente.
	_, _ = s.Put(Entry{SQL: "ui"})
	mcpToken, _ := s.PutWithOrigin(Entry{SQL: "mcp"}, OriginMCP)
	_, _ = s.RequestApproval(mcpToken)

	pend := s.ListPending()
	if len(pend) != 1 || pend[0].Token != mcpToken {
		t.Fatalf("ListPending debía traer solo el token mcp pendiente, got %+v", pend)
	}
}
