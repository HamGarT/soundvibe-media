package httpserver_test

import (
	"net/http"
	"testing"
)

// TestRutasRegistradas comprueba que cada ruta publica existe.
//
// Existe por un bug real: el handler de /rooms/active estaba escrito y
// compilaba — en Go un metodo sin usar no es error — pero nunca se registro en
// el router. El servicio respondia 404 a la pantalla de amigos, que lo trataba
// como "no hay nadie transmitiendo" y mostraba a todos desconectados. Nada
// fallo: ni el build, ni los tests, ni el despliegue.
//
// Se afirma "no es 404", no un status concreto: lo que se esta cuidando es que
// la ruta este cableada, y el comportamiento de cada una ya lo cubren sus
// propios tests.
func TestRutasRegistradas(t *testing.T) {
	rutas := []struct {
		metodo string
		ruta   string
	}{
		{http.MethodGet, "/health"},
		{http.MethodPost, "/rooms/join"},
		{http.MethodGet, "/rooms/active"},
		{http.MethodGet, "/rooms/broadcast"},
		{http.MethodGet, "/rooms/presence"},
		{http.MethodPost, "/internal/revoke"},
	}

	h := newHarness(t)

	for _, caso := range rutas {
		t.Run(caso.metodo+" "+caso.ruta, func(t *testing.T) {
			req, err := http.NewRequest(caso.metodo, h.server.URL+caso.ruta, nil)
			if err != nil {
				t.Fatalf("no se pudo armar el request: %v", err)
			}

			resp, err := h.server.Client().Do(req)
			if err != nil {
				t.Fatalf("el request fallo: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusNotFound {
				t.Errorf("%s %s responde 404: la ruta no esta registrada en el router",
					caso.metodo, caso.ruta)
			}
		})
	}
}
