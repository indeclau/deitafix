# Contribuir a Deitafix

¡Gracias por tu interés! Esta guía explica cómo levantar el entorno, el flujo de ramas y cómo enviar cambios.

## Antes de empezar

Al abrir un Pull Request aceptás el [Contributor License Agreement](CLA.md). En resumen: conservás la autoría de tu código, pero le otorgás al proyecto una licencia amplia (incluida la posibilidad de relicenciar a futuro).

## Levantar el entorno local

Requisitos: Docker y Go (1.22+).

```powershell
# 1. Levantar las bases de datos de desarrollo (Postgres + MariaDB con seed)
docker compose up -d
docker compose ps        # ambos deben figurar como "healthy"

# 2. Correr los tests
go test ./...

# 3. Correr el servicio
go run ./cmd/deitafix
```

## Flujo de ramas (trunk-based)

Usamos **trunk-based con ramas de vida corta**. `main` es la única rama de larga vida y **siempre debe estar desplegable**: todo lo que está ahí compila, pasa los tests y podría releasearse.

- **Nunca** se commitea directo a `main`.
- Cada cambio sale de `main`, vive pocos días y vuelve por Pull Request.
- Nombrá la rama con un prefijo según el tipo de cambio:

| Prefijo | Uso |
|---|---|
| `feat/` | nueva funcionalidad (ej. `feat/nl-to-sql`) |
| `fix/` | corrección de bug (ej. `fix/preview-token-ttl`) |
| `docs/` | documentación (ej. `docs/readme-quickstart`) |
| `chore/` | mantenimiento, CI, deps (ej. `chore/ci-matrix`) |

## Pull Requests

1. Abrí el PR contra `main` con una descripción clara del **qué** y el **por qué**.
2. El CI (GitHub Actions) debe pasar en **verde** sobre los dos motores. Un PR con CI en rojo no se mergea.
3. Al mergear se usa **squash merge**: todos los commits del PR se aplastan en uno solo sobre `main`, para mantener un historial lineal y limpio.

## Conventional Commits

El título del PR (y el commit final por squash) sigue [Conventional Commits](https://www.conventionalcommits.org/):

```
feat: agregar modo operación acotada para UPDATE
fix: rechazar DELETE sin WHERE en el parser de MySQL
docs: aclarar setup del usuario restringido
chore: agregar matrix de CI para Postgres y MariaDB
```

Esto mantiene el historial legible y habilita changelogs y versionado semántico automáticos más adelante.

## Releases

Las versiones se marcan con **tags SemVer** sobre `main` (`v0.1.0`, `v0.2.0`, ...), no con ramas de release. Cada tag dispara la construcción y publicación de la imagen Docker.

## Reportar bugs o proponer ideas

Abrí un **issue** antes de mandar un PR grande, así lo discutimos primero y evitás trabajo que después haya que rehacer. Para bugs, incluí pasos para reproducir, motor y versión.
