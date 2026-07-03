// Package store implementa el estado en memoria del flujo preview → confirm.
//
// Entre el preview y el confirm, el servicio guarda la sentencia ya validada
// (nunca vuelve a aceptar SQL en el confirm). Cada entrada tiene:
//
//   - un token opaco de un solo uso;
//   - un TTL: pasado ese tiempo, el token deja de ser válido;
//   - consumo atómico: Take invalida el token al recuperarlo, de modo que no se
//     pueda confirmar dos veces la misma operación.
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

// ErrNotFound indica que el token no existe, ya fue consumido o expiró.
var ErrNotFound = errors.New("store: token inexistente o expirado")

// Entry es la operación validada asociada a un token, lista para confirmar.
type Entry struct {
	// SQL es la sentencia final a ejecutar en el confirm (idéntica a la del
	// preview).
	SQL string

	// Args son los parámetros de la sentencia (para el modo operación acotada;
	// vacío en SQL crudo).
	Args []any

	// Table es la tabla objetivo, para el resumen.
	Table string

	// AffectedRows son las filas medidas en el preview, para poder mostrarlas
	// de nuevo o auditarlas.
	AffectedRows int64
}

// item es una Entry con su instante de expiración.
type item struct {
	entry     Entry
	expiresAt time.Time
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

// Put guarda una Entry y devuelve un token nuevo de un solo uso.
func (s *Store) Put(e Entry) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[token] = item{
		entry:     e,
		expiresAt: s.now().Add(s.ttl),
	}
	return token, nil
}

// Take recupera la Entry asociada al token y lo invalida de forma atómica.
// Devuelve ErrNotFound si el token no existe, ya fue consumido o expiró.
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
