# Política de seguridad

Deitafix ejecuta escrituras sobre bases de producción, así que la seguridad es el
centro del proyecto. Agradecemos los reportes responsables de vulnerabilidades.

## Reportar una vulnerabilidad

**No abras un issue público** para una vulnerabilidad de seguridad.

Usá el canal privado de GitHub: en la pestaña **Security** del repositorio →
**Report a vulnerability** (GitHub Security Advisories). Esto abre un aviso
privado visible solo para vos y los mantenedores.

Si no podés usar ese canal, abrí un issue mínimo pidiendo un medio de contacto
privado, **sin** incluir detalles del fallo.

En tu reporte, incluí en lo posible:

- una descripción del problema y su impacto;
- pasos para reproducirlo (o una prueba de concepto);
- la versión / commit afectado;
- cualquier mitigación que conozcas.

### Qué esperar

- **Acuse de recibo**: dentro de los primeros días hábiles.
- **Evaluación**: confirmamos o descartamos el reporte y coordinamos con vos una
  fecha de divulgación.
- **Crédito**: si querés, te acreditamos en el aviso y en el `CHANGELOG`.

Como proyecto self-hosted, no operamos infraestructura con datos de terceros: la
corrección se entrega como una nueva versión y cada quien actualiza su despliegue.

## Versiones soportadas

Se da soporte de seguridad a la última versión estable publicada. Al estar en el
ciclo `1.x`, las correcciones se publican sobre la línea `1.x` vigente.

| Versión | Soportada |
|---------|-----------|
| `1.x`   | ✅        |
| `< 1.0` | ❌        |

## Modelo de amenazas (resumen)

Deitafix se apoya en **defensa en profundidad**: ninguna garantía descansa en una
sola capa. El detalle técnico completo está en
[`docs/SECURITY.md`](docs/SECURITY.md). En resumen, las capas son:

1. **Guardas de sentencia** — parser real de cada motor (no regex) + reglas puras:
   rechazan `UPDATE`/`DELETE` sin `WHERE`, multi-statement, DDL/`DROP`/`TRUNCATE`,
   `INSERT ... SELECT` y tablas fuera de la whitelist. Los comentarios que ocultan
   la intención no engañan al parser.
2. **Tope de filas** (`MAX_AFFECTED_ROWS`) — el impacto se mide en una transacción
   con `ROLLBACK` antes de confirmar; si supera el tope, el preview aborta. Es la
   red contra escrituras masivas, incluso con un `WHERE` trivial (`WHERE 1=1`).
3. **Usuario restringido de la base** — la `DATABASE_URL` apunta a un usuario con
   grants mínimos (sin DDL, solo las tablas de la whitelist). Aunque una guarda
   fallara, el motor niega la operación. Ver
   [`docs/RESTRICTED-USER.md`](docs/RESTRICTED-USER.md).
4. **Flujo preview → confirm con token** — el `confirm` acepta solo un **token** de
   un solo uso, nunca SQL; la ejecución la dispara un humano. Un agente de IA jamás
   ejecuta por su cuenta (human-in-the-loop forzado a nivel servidor).

## Buenas prácticas para quien despliega

- Usá **siempre** el usuario restringido en `DATABASE_URL`, nunca el owner/superusuario.
- Configurá `MAX_AFFECTED_ROWS` acorde a tu tolerancia.
- Mantené la whitelist de tablas lo más chica posible.
- No expongas el servicio a Internet sin autenticación (`UI_AUTH_TOKEN`) y TLS.
- Tratá el `AI_API_KEY` y el `MCP_AUTH_TOKEN` como secretos.
