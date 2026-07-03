// Package ui sirve el frontend web mobile-first embebido en el binario.
//
// Toda la UI (HTML, CSS y Alpine.js) se compila dentro del binario con go:embed,
// de modo que el mismo proceso que sirve la API sirve también la interfaz: sin
// build step externo, sin CDN, sin servidor aparte. Es una herramienta de
// emergencia y self-hosted, así que no puede depender de internet.
//
// La UI no rompe ninguna garantía de seguridad del core: el confirm manda solo
// el token del preview (nunca re-envía SQL), el confirm siempre lo aprieta un
// humano, y toda validación viene del backend y se muestra tal cual.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// assets embebe la UI completa: index.html en la raíz y los estáticos en static/
// (Alpine.js vendoreado). No se embebe embed.go ni los tests.
//
//go:embed index.html static
var assets embed.FS

// staticFS es el subárbol static/ expuesto como fs.FS, para servir los estáticos
// (Alpine.js) bajo la ruta /static/.
var staticFS = mustSub(assets, "static")

// indexRoutes son las rutas que sirven la single-page (el mismo HTML). La SPA
// decide qué vista mostrar según la ruta:
//   - "/"          → pantalla de entrada (preview → confirm).
//   - "/approvals" → pantalla "Aprobaciones pendientes" (superficie humana a la
//     que apunta la herramienta MCP confirm).
var indexRoutes = map[string]bool{
	"/":          true,
	"/approvals": true,
}

// Handler construye el http.Handler que sirve la UI embebida:
//
//   - GET /            → index.html (pantalla de entrada).
//   - GET /approvals   → index.html (pantalla de aprobaciones pendientes).
//   - GET /static/...  → estáticos embebidos (Alpine.js).
//
// El engine que se pasa es el motor real del servidor (postgres|mysql), que la
// página muestra como indicador read-only: el motor es propiedad del servidor
// (a qué base apunta DATABASE_URL), no algo elegible por request.
func Handler(engine string) http.Handler {
	mux := http.NewServeMux()

	index := renderIndex(engine)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// El ServeMux enruta "/" como catch-all; solo servimos el index en las
		// rutas conocidas de la SPA y devolvemos 404 para cualquier otra cosa,
		// para no dar falso 200 a rutas inexistentes.
		if !indexRoutes[r.URL.Path] {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})

	// Estáticos con cache larga: el nombre del archivo lleva la versión anclada
	// de Alpine, así que es seguro cachearlo agresivamente.
	fileServer := http.FileServer(http.FS(staticFS))
	mux.Handle("/static/", cacheControl(http.StripPrefix("/static/", fileServer)))

	return mux
}

// renderIndex inyecta el motor real del servidor en el HTML embebido, escapando
// el valor para no romper el atributo aunque el motor traiga algo inesperado.
func renderIndex(engine string) []byte {
	raw, err := assets.ReadFile("index.html")
	if err != nil {
		// index.html está embebido: si falta, es un error de build, no de runtime.
		panic("ui: index.html no embebido: " + err.Error())
	}
	return []byte(strings.Replace(string(raw), "{{ENGINE}}", htmlAttrEscape(engine), 1))
}

// cacheControl agrega Cache-Control a los estáticos versionados.
func cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		next.ServeHTTP(w, r)
	})
}

// htmlAttrEscape escapa lo mínimo para inyectar un valor de servidor dentro de
// un atributo HTML entre comillas dobles sin romperlo ni permitir inyección.
func htmlAttrEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}

// mustSub devuelve el subárbol dir de fsys o entra en pánico: la estructura de
// embed es fija y conocida en tiempo de compilación.
func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic("ui: subárbol embebido inválido " + dir + ": " + err.Error())
	}
	return sub
}
