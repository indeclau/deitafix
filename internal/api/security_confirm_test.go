// Copyright 2026 indeclau. Licensed under the Apache License, Version 2.0.
// See the LICENSE file in the repository root for the full license text.

package api

import (
	"net/http"
	"testing"
)

// El contrato de seguridad más importante del confirm: NUNCA acepta SQL. Se
// ejecuta exactamente lo previsualizado (lo que quedó guardado en el token),
// nunca algo que venga en el cuerpo del confirm. Estos tests lo demuestran de
// punta a punta sobre el router real.

// TestConfirmRejectsSQLInBody verifica que un cuerpo de /confirm con un campo
// "sql" (además del token) se RECHAZA con 400, en vez de ejecutarse o de
// ignorarse a medias. El decoder usa DisallowUnknownFields, así que un intento
// de colar SQL por el confirm ni siquiera se procesa.
func TestConfirmRejectsSQLInBody(t *testing.T) {
	eng := &fakeEngine{affected: 3}
	srv := newTestServer(t, eng, true, 50)

	// Preview legítimo para tener un token válido.
	status, body := postJSON(t, srv.URL+"/preview",
		`{"sql":"UPDATE CollectionBox SET status = 1 WHERE id = 42"}`)
	if status != http.StatusOK {
		t.Fatalf("preview status = %d, body = %v", status, body)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatalf("preview no devolvió token: %v", body)
	}

	// Confirm con un "sql" malicioso adjunto al token válido. DisallowUnknownFields
	// hace que el cuerpo se rechace con 400 antes de tocar nada.
	status, _ = postJSON(t, srv.URL+"/confirm",
		`{"token":"`+token+`","sql":"DELETE FROM CollectionBox"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("confirm con sql en el body status = %d, want 400", status)
	}

	// Como el confirm se rechazó, el token NO se consumió: la operación
	// previsualizada sigue disponible para un confirm legítimo (solo token).
	status, body = postJSON(t, srv.URL+"/confirm", `{"token":"`+token+`"}`)
	if status != http.StatusOK {
		t.Fatalf("confirm legítimo tras el rechazo status = %d, body = %v", status, body)
	}

	// Y lo ejecutado es EXACTAMENTE lo previsualizado (el UPDATE), nunca el DELETE
	// que venía en el cuerpo rechazado.
	if eng.lastConfirmSQL == "" {
		t.Fatal("no se ejecutó ninguna sentencia en el confirm")
	}
	if got := eng.lastConfirmSQL; got != "UPDATE CollectionBox SET status = 1 WHERE id = 42" {
		t.Fatalf("se ejecutó %q; se esperaba exactamente el UPDATE previsualizado", got)
	}
}

// TestConfirmIgnoresUnknownFields refuerza que cualquier campo extra en el
// cuerpo del confirm (no solo "sql") se rechaza, cerrando la superficie a
// payloads inesperados.
func TestConfirmRejectsUnknownFields(t *testing.T) {
	eng := &fakeEngine{affected: 1}
	srv := newTestServer(t, eng, true, 50)

	status, body := postJSON(t, srv.URL+"/preview",
		`{"sql":"DELETE FROM CollectionBox WHERE id = 7"}`)
	if status != http.StatusOK {
		t.Fatalf("preview status = %d, body = %v", status, body)
	}
	token, _ := body["token"].(string)

	// Campo arbitrario extra: rechazado con 400.
	status, _ = postJSON(t, srv.URL+"/confirm",
		`{"token":"`+token+`","engine":"postgres"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("confirm con campo extra status = %d, want 400", status)
	}
}
