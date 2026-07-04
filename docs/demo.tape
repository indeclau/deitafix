# Script de vhs para generar el GIF de demo de Deitafix (docs/demo.gif).
#
# vhs (https://github.com/charmbracelet/vhs) graba una terminal ejecutando estos
# comandos y produce un GIF determinístico: mismo script -> mismo GIF.
#
# Requisitos para regenerar:
#   1. Instalar vhs:  https://github.com/charmbracelet/vhs#installation
#   2. Levantar el stack de desarrollo (Postgres + MariaDB con seed) y el
#      servicio en localhost:8080:
#        docker compose up -d
#        $env:DATABASE_URL = "postgres://prod_datafix:dev_datafix_pw@localhost:5432/deitafix_dev"
#        $env:DATAFIX_ENABLED = "true"
#        $env:TABLE_WHITELIST = "CollectionBox"
#        go run ./cmd/deitafix
#   3. Renderizar el GIF (desde la raíz del repo):
#        vhs docs/demo.tape
#
# El flujo demostrado es el corazón de Deitafix: preview (mide el impacto sin
# persistir) -> confirm (ejecuta solo el token previsualizado, nunca SQL nuevo).

Output docs/demo.gif

Set Shell "bash"
Set FontSize 20
Set Width 1200
Set Height 640
Set Padding 20
Set Theme "Dracula"
Set TypingSpeed 45ms

# --- Preview: valida, mide impacto, devuelve un token de un solo uso ---
Type "# 1) Preview: valida, mide el impacto y devuelve un token (nada se persiste)"
Enter
Sleep 800ms
Type `curl -s localhost:8080/preview -d '{"sql":"UPDATE \"CollectionBox\" SET status = 1 WHERE id = 42"}' | jq`
Enter
Sleep 2500ms

# Muestra la respuesta: token + affected_rows + summary
Type "# -> { token, affected_rows: 1, summary: \"UPDATE ... afectaria 1 fila(s)\" }"
Enter
Sleep 2000ms

# --- Confirm: ejecuta SOLO el token (nunca SQL nuevo) ---
Type "# 2) Confirm: se manda SOLO el token. Se ejecuta exactamente lo previsualizado."
Enter
Sleep 800ms
Type `TOKEN=$(curl -s localhost:8080/preview -d '{"sql":"UPDATE \"CollectionBox\" SET status = 1 WHERE id = 42"}' | jq -r .token)`
Enter
Sleep 1200ms
Type `curl -s localhost:8080/confirm -d "{\"token\":\"$TOKEN\"}" | jq`
Enter
Sleep 2500ms

Type "# -> { affected_rows: 1, summary: \"...\" }  ✔ commit"
Enter
Sleep 2500ms

# --- Guarda en acción: un UPDATE sin WHERE es rechazado ---
Type "# La guarda en accion: un UPDATE sin WHERE se rechaza (422), nunca toca la base"
Enter
Sleep 800ms
Type `curl -s -o /dev/null -w "%{http_code}\n" localhost:8080/preview -d '{"sql":"UPDATE \"CollectionBox\" SET status = 1"}'`
Enter
Sleep 1500ms
Type "# -> 422  (UPDATE/DELETE sin WHERE: rechazado por el parser real)"
Enter
Sleep 3000ms
