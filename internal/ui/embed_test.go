package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// do hace un GET contra el Handler de UI y devuelve la respuesta y su cuerpo.
func do(t *testing.T, h http.Handler, path string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("leyendo cuerpo de %s: %v", path, err)
	}
	_ = res.Body.Close()
	return res, string(body)
}

func TestServesIndexOnRoot(t *testing.T) {
	res, body := do(t, Handler("postgres"), "/")

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html…", ct)
	}
	// Es el HTML embebido, no un placeholder vacío.
	if !strings.Contains(body, "<title>Deitafix</title>") {
		t.Fatalf("cuerpo no parece el index embebido: %.80q", body)
	}
	// El flujo preview → confirm debe estar cableado en la página.
	for _, want := range []string{"doPreview", "doConfirm", "/static/alpine.min.js"} {
		if !strings.Contains(body, want) {
			t.Fatalf("el index no contiene %q", want)
		}
	}
}

func TestInjectsEngineIntoIndex(t *testing.T) {
	_, body := do(t, Handler("mysql"), "/")

	if !strings.Contains(body, `data-engine="mysql"`) {
		t.Fatalf("el motor no se inyectó en el HTML: falta data-engine=\"mysql\"")
	}
	// El placeholder no debe quedar sin reemplazar.
	if strings.Contains(body, "{{ENGINE}}") {
		t.Fatalf("quedó el placeholder {{ENGINE}} sin reemplazar")
	}
}

func TestEngineValueIsEscaped(t *testing.T) {
	// Un valor con caracteres peligrosos no debe romper el atributo ni permitir
	// inyección de HTML.
	_, body := do(t, Handler(`p"><script>x`), "/")

	if strings.Contains(body, `<script>x`) {
		t.Fatalf("el valor del motor no fue escapado: %q", body)
	}
	if !strings.Contains(body, "&quot;") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("no se ve el escapado esperado en el atributo")
	}
}

func TestServesVendoredAlpine(t *testing.T) {
	res, body := do(t, Handler("postgres"), "/static/alpine.min.js")

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/alpine.min.js status = %d, want 200", res.StatusCode)
	}
	if len(body) == 0 {
		t.Fatalf("alpine.min.js vino vacío")
	}
	// Es Alpine de verdad: el bootstrap expone window.Alpine.
	if !strings.Contains(body, "window.Alpine") {
		t.Fatalf("el estático servido no parece Alpine.js")
	}
	// Se cachea agresivamente porque el nombre lleva versión anclada.
	if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Fatalf("Cache-Control = %q, want cache larga", cc)
	}
}

func TestUnknownPathReturns404(t *testing.T) {
	// El catch-all no debe dar un falso 200 a rutas inexistentes.
	res, _ := do(t, Handler("postgres"), "/no-existe")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /no-existe status = %d, want 404", res.StatusCode)
	}
}

func TestHTMLAttrEscape(t *testing.T) {
	cases := map[string]string{
		"postgres": "postgres",
		`a"b`:      "a&quot;b",
		"a<b>c":    "a&lt;b&gt;c",
		"a&b":      "a&amp;b",
		"a'b":      "a&#39;b",
	}
	for in, want := range cases {
		if got := htmlAttrEscape(in); got != want {
			t.Errorf("htmlAttrEscape(%q) = %q, want %q", in, got, want)
		}
	}
}
