# syntax=docker/dockerfile:1

# Dockerfile multi-stage de Deitafix.
#
# Restricción crítica: pg_query_go envuelve la librería C libpg_query, así que
# el binario NO puede compilarse sin cgo (CGO_ENABLED=1). Eso obliga a tener un
# toolchain de C en el stage de build (la imagen golang de Debian ya trae gcc).
#
# El binario resultante enlaza dinámicamente contra glibc, por eso la imagen
# final es distroless/base-debian12 (trae glibc) en vez de distroless/static.
# Se verificó con `ldd`/`file` que corre en esa base.

# ---- Stage build ----
# Versión de Go alineada con el go.mod (go 1.22). Imagen Debian: incluye gcc.
FROM golang:1.22 AS build

WORKDIR /src

# Cachear módulos en una capa aparte: solo se invalida si cambian go.mod/go.sum,
# no en cada cambio de código.
COPY go.mod go.sum ./
RUN go mod download

# Ahora sí el código fuente.
COPY . .

# cgo obligatorio por libpg_query. -trimpath y -ldflags "-s -w" achican el
# binario quitando rutas de build y tabla de símbolos/DWARF.
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=1 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/deitafix ./cmd/deitafix

# ---- Stage final ----
# base-debian12 (no static) porque el binario cgo enlaza glibc dinámicamente.
# La variante :nonroot corre como usuario sin privilegios (uid 65532).
FROM gcr.io/distroless/base-debian12:nonroot

WORKDIR /
COPY --from=build /out/deitafix /usr/local/bin/deitafix

# Puerto HTTP por defecto (config.defaultPort).
EXPOSE 8080

# Ya viene como nonroot por la variante de la imagen, explícito por claridad.
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/deitafix"]
