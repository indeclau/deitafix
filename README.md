# Deitafix

Un servicio open-source y self-hosted para ejecutar **escrituras ocasionales sobre una base de datos de producción** (`UPDATE` / `DELETE` / `INSERT`) de forma segura, con un flujo **preview → confirm** donde nada se ejecuta a ciegas.

Pensado como punto único y controlado para reemplazar el "conectarse directo con credenciales a producción", con salvaguardas a nivel motor y una capa de IA/agéntica opcional.

---

## El problema

En muchos equipos, un `UPDATE` o `DELETE` puntual sobre producción implica que alguien se conecte con credenciales completas a la base y ejecute SQL a mano. Eso es la peor superficie de riesgo posible: un `DELETE` sin `WHERE`, un token filtrado o una persona apurada a las 3am, y hay incidente.

El problema real no es "ejecutar SQL", es **quién puede ejecutar qué, con qué límites, y viendo el impacto antes de confirmar.**

## Cómo funciona

Todo cambio pasa por dos pasos:

1. **Preview** — la sentencia se parsea con el parser real del motor, se valida contra las guardas, se ejecuta dentro de una transacción para medir el impacto (**filas afectadas**) y se hace `ROLLBACK`. Devuelve un **token de un solo uso** con TTL.
2. **Confirm** — se envía **solo el token** (nunca SQL nuevo). El servicio recupera la sentencia ya validada, la ejecuta y hace `COMMIT`.

> El `confirm` no acepta SQL: solo el token. Eso garantiza que se ejecuta **exactamente** lo que se previsualizó.

## Pilares de seguridad

- **Contención a nivel motor** — el servicio se conecta con un usuario de base de datos dedicado y restringido: whitelist de tablas, solo datos, sin DDL / `DROP` / `TRUNCATE`. Si todo lo demás falla, la base limita el daño.
- **Preview obligatorio** — ninguna operación se ejecuta sin ver antes su impacto real.
- **Guardas de sentencia** — parser real (no regex) que rechaza `UPDATE` / `DELETE` sin `WHERE`, aplica un tope de filas afectadas y valida tabla + operación contra la whitelist.

---

## Alcance v1

**Incluido:**

- Motores: **PostgreSQL** y **MySQL / MariaDB**
- Operaciones: `UPDATE`, `DELETE`, `INSERT` (solo `VALUES` explícitos)
- Dos modos de entrada: **SQL crudo** (con guardas) y **operación acotada** (tabla + campos + where)
- Flujo **preview → confirm** en dos pasos, estado en memoria del proceso (TTL por token)
- **UI web mobile-first** embebida en el binario (emergencias desde el celular)
- **Capa de IA** (ver abajo)
- Feature flag on/off + usuario de base de datos restringido

**Fuera de alcance (v2+):**

- Aprobación four-eyes (revisión humana previa)
- Audit log persistente e inmutable
- `INSERT ... SELECT`
- Rollback más allá de la transacción
- Motores adicionales (Oracle, SQL Server)

---

## La capa de IA

Toda sugerencia de la IA pasa por **las mismas guardas y el mismo preview → confirm**. La IA nunca saltea la seguridad: solo propone.

- **Servidor MCP** — expone `preview` y `confirm` como herramientas MCP. Un agente puede *proponer* escrituras, obligado a pasar por las salvaguardas. Es el ángulo central: hacer seguro que un agente de IA toque una base de producción.
- **NL → SQL** — describís la intención en lenguaje natural ("borrá el registro X del cliente Y") y la IA propone la sentencia candidata. Ideal para el caso de emergencia desde el celular.
- **Explicación de impacto** — en el preview, la IA traduce "47 filas afectadas" a lenguaje claro y marca riesgo.
- **Revisor IA** — señala patrones sospechosos en la sentencia (segundo par de ojos parcial).

> 🔒 **No negociable:** el `confirm` lo aprieta **siempre un humano**. El agente puede previsualizar; ejecutar es decisión de una persona (human-in-the-loop). La capa de IA degrada de forma limpia si no hay API key configurada.

---

## Quickstart (Docker)

```powershell
docker run --rm `
  -p 8080:8080 `
  -e DATABASE_URL="postgres://prod_datafix:CAMBIAR@host:5432/midb" `
  -e DATAFIX_ENABLED="true" `
  -e MAX_AFFECTED_ROWS="50" `
  Deitafix:latest
```

Abrí `http://localhost:8080` para la UI, o usá la API directamente.

### Variables de entorno

| Variable | Descripción |
|---|---|
| `DATABASE_URL` | Conexión con el usuario **restringido** (nunca el de la app) |
| `DATAFIX_ENABLED` | Feature flag. `false` deja el servicio apagado |
| `MAX_AFFECTED_ROWS` | Tope de filas; si se supera, aborta |
| `AI_API_KEY` | *(Opcional)* habilita la capa de IA |

---

## Ejemplo de uso (API)

```powershell
# 1. Preview: valida, mide impacto, devuelve token
curl -X POST http://localhost:8080/preview `
  -H "Content-Type: application/json" `
  -d '{"engine":"postgres","sql":"UPDATE \"CollectionBox\" SET status = 1 WHERE id = 42"}'

# Respuesta: { "token": "abc123", "affected_rows": 1, "summary": "..." }

# 2. Confirm: ejecuta solo el token
curl -X POST http://localhost:8080/confirm `
  -H "Content-Type: application/json" `
  -d '{"token":"abc123"}'
```

---

## Configurar el usuario restringido

La pieza más importante vive en la base, no en el código. Ejemplo PostgreSQL:

```sql
CREATE USER prod_datafix WITH PASSWORD 'CAMBIAR_password_fuerte';
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM prod_datafix;
GRANT USAGE ON SCHEMA public TO prod_datafix;
-- Whitelist explícita, una tabla por vez:
GRANT SELECT, INSERT, UPDATE, DELETE ON "CollectionBox" TO prod_datafix;
```

**Whitelist, nunca blacklist.** Nombrás una por una las tablas que se pueden tocar. Sin DDL, sin `DROP`, sin `TRUNCATE`.

---

## Stack

- **Go** + [`chi`](https://github.com/go-chi/chi) — API HTTP
- [`pgx`](https://github.com/jackc/pgx) / [`go-sql-driver/mysql`](https://github.com/go-sql-driver/mysql) — drivers
- [`pg_query_go`](https://github.com/pganalyze/pg_query_go) / [`pingcap/tidb/parser`](https://github.com/pingcap/tidb) — parsers reales por motor
- **Alpine.js** + CSS mobile-first, embebido con `embed`
- Docker multi-stage → binario único que sirve API + UI

---

## Roadmap sugerido

Para llegar a un hito usable sin construir todo de golpe:

1. **Core** — preview → confirm, dos motores, guardas, usuario restringido, UI.
2. **MCP** — servidor MCP sobre el core ya seguro.
3. **Features LLM** — NL → SQL, explicación de impacto, revisor IA.
4. **v2** — aprobación four-eyes, audit log persistente.

---

## Contribuir

Las contribuciones son bienvenidas. Abrí un issue para discutir cambios grandes antes de un PR.

## Licencia

[Apache License 2.0](LICENSE). Permisiva (uso comercial, modificación y redistribución permitidos) e incluye una concesión explícita de patentes.

Las contribuciones se aceptan bajo el [Contributor License Agreement](CLA.md): al abrir un PR, aceptás sus términos. Esto mantiene la potestad de relicenciar el proyecto a futuro (por ejemplo, un modelo open-core), sin necesidad del permiso individual de cada contribuidor.
