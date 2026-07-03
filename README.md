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

La imagen se publica en GHCR al taggear una versión. Corré la última así (PowerShell):

```powershell
docker run --rm `
  -p 8080:8080 `
  -e DATABASE_URL="postgres://prod_datafix:CAMBIAR@host:5432/midb" `
  -e DEITAFIX_ENGINE="postgres" `
  -e DATAFIX_ENABLED="true" `
  -e MAX_AFFECTED_ROWS="50" `
  -e TABLE_WHITELIST="CollectionBox" `
  ghcr.io/indeclau/deitafix:latest
```

> El nombre de la imagen va en **minúscula** (`deitafix`), como exige GHCR.

Comprobá que está viva y lista, y probá la API:

```powershell
# Liveness (no toca la base) y readiness (hace ping a la base):
curl.exe http://localhost:8080/healthz
curl.exe http://localhost:8080/readyz
```

Abrí `http://localhost:8080` para la UI, o usá la API directamente (más abajo).

### UI web (mobile-first)

El binario sirve, en el mismo proceso que la API, una **UI web mobile-first embebida** para el caso de emergencia desde el celular. No requiere build step, servidor aparte ni CDN: Alpine.js y el CSS van vendoreados dentro del binario con `embed`.

El flujo en pantalla es el mismo **preview → confirm** del backend, en dos pasos:

1. **Entrada** — elegís SQL crudo u operación acotada (tabla + campos + where) y tocás **Preview**. El motor de la base se muestra como indicador *read-only* (es propiedad del servidor, no se elige por request).
2. **Impacto** — ves las **filas afectadas** y la sentencia exacta que se va a ejecutar. **Confirmar** manda **solo el token** del preview (nunca SQL) y lo aprieta siempre un humano; **Volver** descarta el token sin ejecutar nada.

Los errores de guardas (p. ej. `DELETE` sin `WHERE`, tope de filas superado, tabla fuera de la whitelist) vienen del backend y se muestran tal cual: la UI no agrega validaciones propias que reemplacen a las guardas del core.

### Variables de entorno

| Variable | Descripción |
|---|---|
| `DATABASE_URL` | Conexión con el usuario **restringido** (nunca el de la app) |
| `DEITAFIX_ENGINE` | Motor: `postgres` \| `mysql`. Si se omite, se infiere de `DATABASE_URL` |
| `DATAFIX_ENABLED` | Feature flag. `false` deja el servicio apagado |
| `MAX_AFFECTED_ROWS` | Tope de filas; si se supera, aborta |
| `TABLE_WHITELIST` | Tablas permitidas, separadas por coma (además de la contención en la base) |
| `AI_API_KEY` | *(Opcional)* habilita la capa de IA |

---

## Ejemplo de uso (API)

```powershell
# 1. Preview: valida, mide impacto, devuelve token.
$preview = Invoke-RestMethod -Method Post -Uri http://localhost:8080/preview `
  -ContentType "application/json" `
  -Body '{"sql":"UPDATE \"CollectionBox\" SET status = 1 WHERE id = 42"}'
$preview
# token affected_rows summary
# ----- ------------- -------
# abc123             1 UPDATE sobre "CollectionBox" afectaría 1 fila(s)...

# 2. Confirm: ejecuta SOLO el token (nunca SQL nuevo).
Invoke-RestMethod -Method Post -Uri http://localhost:8080/confirm `
  -ContentType "application/json" `
  -Body (@{ token = $preview.token } | ConvertTo-Json)
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

## Despliegue en Kubernetes

En [`k8s/`](k8s/) hay manifiestos mínimos: `Deployment`, `Service` (ClusterIP al 8080) y un `Secret` de ejemplo.

```powershell
# 1. Copiá el Secret de ejemplo y completá los valores reales.
#    DATABASE_URL debe usar el usuario RESTRINGIDO (nunca el de la app).
Copy-Item k8s/secret.example.yaml k8s/secret.yaml
#    editá k8s/secret.yaml con tu editor...

# 2. Aplicá el Secret (con valores reales) y el resto de los manifiestos.
kubectl apply -f k8s/secret.yaml
kubectl apply -f k8s/deployment.yaml -f k8s/service.yaml
```

El `Deployment` usa `livenessProbe` → `/healthz` y `readinessProbe` → `/readyz`, corre como usuario **nonroot** con el filesystem raíz de solo lectura, y toma la imagen de `ghcr.io/indeclau/deitafix`. Ajustá el tag a la versión que quieras desplegar.

> `secret.example.yaml` solo trae placeholders. El `secret.yaml` con credenciales reales **no se commitea**.

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
