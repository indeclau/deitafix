package engine

import (
	"context"
	"sync"
	"time"
)

// ColumnInfo describe una columna tal como la ve la introspección de esquema:
// nombre y tipo de dato. Es un tipo neutral al motor (postgres/mysql) para que
// la capa de IA lo consuma sin conocer el motor.
type ColumnInfo struct {
	Name string
	Type string
}

// Introspector es la capacidad OPCIONAL de un Engine de exponer el esquema de
// un conjunto de tablas. Se mantiene fuera de la interfaz Engine (la interfaz
// dura del core) a propósito: la introspección solo la usa la capa de IA para
// dar contexto al modelo en NL → SQL, y su ausencia jamás debe afectar la ruta
// segura preview → confirm.
//
// El caller descubre la capacidad con un type-assert (eng.(Introspector)); si el
// motor no la implementa, la IA funciona igual con solo la intención y los
// nombres de tabla.
type Introspector interface {
	// Columns devuelve, para cada tabla pedida, sus columnas. La consulta se
	// acota EXACTAMENTE a las tablas pasadas (las de la whitelist): nunca se
	// introspecciona toda la base. Una tabla que el usuario restringido no puede
	// ver simplemente no aparece en el resultado (sin error).
	Columns(ctx context.Context, tables []string) (map[string][]ColumnInfo, error)
}

// SchemaCache envuelve un Introspector con un cache en memoria con TTL. El
// esquema de la whitelist cambia rara vez; cachearlo evita golpear
// information_schema en cada NL → SQL. Es seguro para uso concurrente.
//
// El cache es best-effort: si la introspección falla, se devuelve el error y no
// se cachea nada (para reintentar la próxima). No hay invalidación explícita;
// el TTL alcanza para el caso de uso (emergencias ocasionales).
type SchemaCache struct {
	introspector Introspector
	ttl          time.Duration

	mu       sync.Mutex
	cached   map[string][]ColumnInfo
	cachedAt time.Time
	key      string // firma del set de tablas cacheado, para invalidar si cambia
	now      func() time.Time
}

// NewSchemaCache envuelve un Introspector con el TTL dado.
func NewSchemaCache(in Introspector, ttl time.Duration) *SchemaCache {
	return &SchemaCache{
		introspector: in,
		ttl:          ttl,
		now:          time.Now,
	}
}

// Columns devuelve el esquema de las tablas pedidas, sirviéndolo del cache si
// está fresco y corresponde al mismo set de tablas. Si el cache expiró o el set
// cambió, reintrospecciona.
func (c *SchemaCache) Columns(ctx context.Context, tables []string) (map[string][]ColumnInfo, error) {
	key := tablesKey(tables)

	c.mu.Lock()
	fresh := c.cached != nil &&
		c.key == key &&
		c.now().Sub(c.cachedAt) < c.ttl
	if fresh {
		out := c.cached
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()

	// Introspección fuera del lock (puede tardar): puede haber dos en vuelo la
	// primera vez, pero el resultado es idéntico y el último gana. Aceptable.
	got, err := c.introspector.Columns(ctx, tables)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.cached = got
	c.cachedAt = c.now()
	c.key = key
	c.mu.Unlock()

	return got, nil
}

// tablesKey arma una firma estable del set de tablas (orden-independiente sería
// ideal, pero el caller pasa siempre la whitelist en el mismo orden, así que una
// concatenación simple alcanza para detectar cambios de configuración).
func tablesKey(tables []string) string {
	var b []byte
	for _, t := range tables {
		b = append(b, t...)
		b = append(b, 0)
	}
	return string(b)
}
