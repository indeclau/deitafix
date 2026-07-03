// Package store implementa el estado en memoria del flujo preview → confirm.
//
// Entre el preview y el confirm, el servicio guarda la sentencia ya validada
// (nunca vuelve a aceptar SQL en el confirm). Cada entrada tiene:
//
//   - un token opaco de un solo uso;
//   - un TTL: pasado ese tiempo, el token deja de ser válido;
//   - un origen (ui | mcp): quién creó el preview, que decide cómo puede
//     ejecutarse (ver Origin);
//   - un estado dentro de su ciclo de vida (ver State);
//   - consumo atómico: las transiciones terminales (Take, Approve, Reject)
//     invalidan el token, de modo que no se pueda confirmar dos veces la misma
//     operación.
//
// El estado vive en el proceso (un map protegido por mutex). Es deliberado para
// v1: no hay persistencia ni coordinación entre réplicas.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// Errores centinela del store.
var (
	// ErrNotFound indica que el token no existe, ya fue consumido o expiró.
	ErrNotFound = errors.New("store: token inexistente o expirado")

	// ErrWrongState indica que el token existe pero no está en el estado que la
	// transición requiere (por ejemplo, aprobar un token ya ejecutado, o tomar
	// por la superficie humana uno que aún no fue propuesto para aprobación).
	ErrWrongState = errors.New("store: el token no está en el estado esperado")
)

// Origin identifica quién originó el preview, lo que determina cómo puede
// ejecutarse la operación.
type Origin string

const (
	// OriginUI son los previews creados desde la superficie humana (UI / API
	// existente). Se ejecutan directamente con el confirm humano.
	OriginUI Origin = "ui"

	// OriginMCP son los previews creados por un agente vía la capa MCP. NUNCA se
	// ejecutan con la credencial del agente: quedan a la espera de aprobación
	// humana explícita. Es el corazón del human-in-the-loop forzado a nivel
	// servidor.
	OriginMCP Origin = "mcp"
)

// State es el estado de una entrada dentro de su ciclo de vida.
//
//	previewed ─┬─ (Take, solo origin=ui) ───────────────► executed
//	           └─ (RequestApproval, solo origin=mcp) ──► pending_approval
//	pending_approval ─┬─ (Approve) ─► executed
//	                  └─ (Reject)  ─► rejected
//
// executed / rejected / expired son estados terminales. Un token expirado no es
// aprobable ni ejecutable.
type State string

const (
	StatePreviewed       State = "previewed"
	StatePendingApproval State = "pending_approval"
	StateExecuted        State = "executed"
	StateRejected        State = "rejected"
	StateExpired         State = "expired"
)

// Entry es la operación validada asociada a un token, lista para confirmar.
type Entry struct {
	// SQL es la sentencia final a ejecutar en el confirm (idéntica a la del
	// preview).
	SQL string

	// Args son los parámetros de la sentencia (para el modo operación acotada;
	// vacío en SQL crudo).
	Args []any

	// Op es la operación de escritura (UPDATE/DELETE/INSERT), para el resumen y
	// para mostrar el impacto en la lista de pendientes.
	Op string

	// Table es la tabla objetivo, para el resumen.
	Table string

	// AffectedRows son las filas medidas en el preview, para poder mostrarlas
	// de nuevo o auditarlas.
	AffectedRows int64

	// Origin indica quién creó el preview (ui | mcp) y por lo tanto cómo puede
	// ejecutarse. Ver Origin.
	Origin Origin

	// State es el estado actual de la entrada. Ver State.
	State State
}

// item es una Entry con su instante de expiración.
type item struct {
	entry     Entry
	expiresAt time.Time
}

// Pending es la vista pública de un token a la espera de aprobación humana, con
// el TTL restante ya calculado. Lo consume la superficie de aprobación.
type Pending struct {
	Token        string
	SQL          string
	Op           string
	Table        string
	AffectedRows int64
	ExpiresIn    time.Duration
}

// Store es un almacén en memoria de tokens con TTL, seguro para uso
// concurrente.
type Store struct {
	mu    sync.Mutex
	items map[string]item
	ttl   time.Duration

	// now es inyectable para los tests; en producción es time.Now.
	now func() time.Time
}

// New crea un Store con el TTL dado.
func New(ttl time.Duration) *Store {
	return &Store{
		items: make(map[string]item),
		ttl:   ttl,
		now:   time.Now,
	}
}

// Put guarda una Entry originada en la superficie humana (origin=ui) y devuelve
// un token nuevo. Es el camino histórico del flujo UI/API; equivale a
// PutWithOrigin(e, OriginUI).
func (s *Store) Put(e Entry) (string, error) {
	return s.PutWithOrigin(e, OriginUI)
}

// PutWithOrigin guarda una Entry con el origen indicado, en estado previewed, y
// devuelve un token nuevo de un solo uso.
func (s *Store) PutWithOrigin(e Entry, origin Origin) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}

	e.Origin = origin
	e.State = StatePreviewed

	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[token] = item{
		entry:     e,
		expiresAt: s.now().Add(s.ttl),
	}
	return token, nil
}

// Take recupera la Entry asociada al token y lo invalida de forma atómica. Es la
// transición terminal del confirm humano directo (superficie humana). Devuelve
// ErrNotFound si el token no existe, ya fue consumido o expiró.
//
// Take no distingue el origen ni el estado a propósito: devuelve la Entry (que
// incluye Origin) para que la capa de aplicación decida si ese token puede
// ejecutarse por esta vía. Así el gating (un token origin=mcp no se ejecuta por
// el confirm humano directo) vive en un solo lugar, con el error adecuado.
func (s *Store) Take(token string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	it, ok := s.items[token]
	if !ok {
		return Entry{}, ErrNotFound
	}
	// Consumo de un solo uso: se borra siempre, esté o no expirado.
	delete(s.items, token)

	if s.now().After(it.expiresAt) {
		return Entry{}, ErrNotFound
	}
	return it.entry, nil
}

// RequestApproval marca un token origin=mcp como pendiente de aprobación humana,
// sin consumirlo ni tocar la base. Es la transición que dispara la herramienta
// MCP confirm: el agente propone, pero no ejecuta.
//
// Devuelve ErrNotFound si el token no existe o expiró, y ErrWrongState si no
// está en estado previewed (por ejemplo si ya se propuso, aprobó o rechazó).
func (s *Store) RequestApproval(token string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	it, ok := s.items[token]
	if !ok {
		return Entry{}, ErrNotFound
	}
	if s.now().After(it.expiresAt) {
		delete(s.items, token)
		return Entry{}, ErrNotFound
	}
	if it.entry.State != StatePreviewed {
		return Entry{}, ErrWrongState
	}

	it.entry.State = StatePendingApproval
	s.items[token] = it
	return it.entry, nil
}

// Peek devuelve la Entry de un token sin consumirlo ni cambiar su estado. Lo usa
// la superficie de aprobación para mostrar el detalle (impacto + SQL exacto)
// antes de que un humano decida. Devuelve ErrNotFound si el token no existe o
// expiró.
func (s *Store) Peek(token string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	it, ok := s.items[token]
	if !ok {
		return Entry{}, ErrNotFound
	}
	if s.now().After(it.expiresAt) {
		delete(s.items, token)
		return Entry{}, ErrNotFound
	}
	return it.entry, nil
}

// Approve consume un token en estado pending_approval y devuelve su Entry para
// que la capa de aplicación la ejecute. Es la transición terminal de la
// aprobación humana: el token se borra (un solo uso), esté o no la ejecución
// posterior a punto de fallar, para que no se pueda aprobar dos veces.
//
// Devuelve ErrNotFound si el token no existe o expiró, y ErrWrongState si no
// está pendiente de aprobación (por ejemplo un token origin=ui aún en
// previewed, o uno ya resuelto).
func (s *Store) Approve(token string) (Entry, error) {
	return s.resolvePending(token)
}

// Reject descarta un token en estado pending_approval sin ejecutarlo. Consume el
// token (un solo uso). Mismos errores que Approve.
func (s *Store) Reject(token string) (Entry, error) {
	return s.resolvePending(token)
}

// resolvePending valida que el token esté pending_approval, lo consume y
// devuelve su Entry. Approve y Reject comparten esta lógica: la diferencia (si
// se ejecuta o se descarta) vive en la capa de aplicación, no en el store.
func (s *Store) resolvePending(token string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	it, ok := s.items[token]
	if !ok {
		return Entry{}, ErrNotFound
	}
	if s.now().After(it.expiresAt) {
		delete(s.items, token)
		return Entry{}, ErrNotFound
	}
	if it.entry.State != StatePendingApproval {
		return Entry{}, ErrWrongState
	}

	// Consumo de un solo uso: se borra al resolver.
	delete(s.items, token)
	return it.entry, nil
}

// ListPending devuelve los tokens origin=mcp en estado pending_approval, con su
// TTL restante. No consume nada. Es la fuente de la pantalla "Aprobaciones
// pendientes". Las entradas expiradas se omiten (y se limpian de paso).
func (s *Store) ListPending() []Pending {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	var out []Pending
	for token, it := range s.items {
		if now.After(it.expiresAt) {
			delete(s.items, token)
			continue
		}
		if it.entry.Origin != OriginMCP || it.entry.State != StatePendingApproval {
			continue
		}
		out = append(out, Pending{
			Token:        token,
			SQL:          it.entry.SQL,
			Op:           it.entry.Op,
			Table:        it.entry.Table,
			AffectedRows: it.entry.AffectedRows,
			ExpiresIn:    it.expiresAt.Sub(now),
		})
	}
	return out
}

// GC elimina las entradas expiradas. Se pensó para llamarse periódicamente
// (por ejemplo desde una goroutine con un ticker) y evitar que tokens que nunca
// se confirman queden ocupando memoria.
func (s *Store) GC() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	for token, it := range s.items {
		if now.After(it.expiresAt) {
			delete(s.items, token)
		}
	}
}

// Len devuelve la cantidad de tokens vivos (útil para tests y métricas).
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

// newToken genera un token opaco criptográficamente aleatorio de 128 bits.
func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
