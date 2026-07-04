# Roadmap

Cada fase es un tag **SemVer** y deja `main` en estado desplegable. Los hitos entregan valor de forma incremental: no hace falta terminar todo para tener algo usable.

Leyenda: cada ítem es una unidad de trabajo (rama corta + PR).

---

## v0.1.0 — Core seguro (el cimiento)

Flujo `preview → confirm` funcionando contra los dos motores, con las guardas y el usuario restringido. Sin UI ni IA todavía: ya es usable vía API.

- [ ] Estructura del proyecto Go (`cmd/`, `internal/`) + `go.mod`
- [ ] Interfaz `Engine` con dos implementaciones: `postgres` y `mysql`
- [ ] Drivers: `pgx` y `go-sql-driver/mysql`
- [ ] Guardas con parser real (`pg_query_go` / parser de TiDB): rechazo de `UPDATE`/`DELETE` sin `WHERE`, whitelist de operaciones, tope de filas
- [ ] `POST /preview`: parseo, validación, transacción + `ROLLBACK`, token en memoria con TTL
- [ ] `POST /confirm`: solo token, ejecución + `COMMIT`, token de un solo uso
- [ ] Dos modos de entrada: SQL crudo y operación acotada
- [ ] Feature flag (`DEITAFIX_ENABLED`) + config por entorno
- [ ] Tests unit (guardas, table-driven) + integración (testcontainers, incluido *permission denied* fuera de whitelist)
- [ ] CI en verde (workflow ya definido)

## v0.2.0 — Docker + Kubernetes (la promesa de "2 minutos")

Desplegar Deitafix de verdad, con un binario único.

- [ ] Dockerfile multi-stage → imagen `distroless` (binario único que sirve API)
- [ ] Workflow de release: build + publicación de imagen al hacer tag
- [ ] Manifiestos Kubernetes (Deployment + Service + Secret para `DATABASE_URL`)
- [ ] Quickstart del README validado ("levanta en 2 minutos")

## v0.3.0 — UI web mobile-first

El caso de emergencia desde el celular.

- [ ] Frontend Alpine.js + CSS mobile-first, embebido con `embed`
- [ ] Pantalla 1: entrada (SQL crudo / operación acotada) → botón Preview
- [ ] Pantalla 2: impacto (filas afectadas + resumen) → botón Confirmar
- [ ] Servido por el mismo binario (API + UI juntas)

## v0.4.0 — Capa MCP

Hacer seguro que un agente de IA toque producción.

- [x] Servidor MCP que expone `preview` y `confirm` como herramientas
- [x] El agente puede previsualizar; el `confirm` sigue siendo **humano** (human-in-the-loop), forzado a nivel servidor (gating por origen del token)
- [x] Superficie de aprobación humana (`/pending`, approve, reject) + UI "Aprobaciones pendientes"
- [x] Documentación de conexión MCP

## v0.5.0 — Features LLM

- [x] NL → SQL: propone la sentencia candidata, que pasa por las guardas + preview
- [x] Explicación de impacto en el preview (lenguaje claro + señal de riesgo)
- [x] Revisor IA: marca patrones sospechosos
- [x] Degradación limpia si no hay `AI_API_KEY`

## v1.0.0 — Endurecimiento y release estable

- [ ] Revisión de seguridad de las guardas y del usuario restringido
- [ ] Documentación completa
- [ ] Cobertura de tests sólida
- [ ] Badges, ejemplos y GIF demo en el README

---

## v2+ — Futuro (fuera del alcance v1)

- Aprobación **four-eyes** (revisión humana previa)
- **Audit log** persistente e inmutable
- `INSERT ... SELECT`
- Rollback más allá de la transacción
- Motores adicionales (Oracle, SQL Server)

---

> El orden es una sugerencia. Core (v0.1.0) es la única dependencia dura del resto; a partir de ahí, UI, MCP y LLM se pueden reordenar según prioridad.
