package httpserver_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// fakeLiveKit imita la API de administracion del SFU. Alcanza con los dos
// metodos twirp que usa la revocacion.
type fakeLiveKit struct {
	server *httptest.Server

	mu sync.Mutex
	// participants es quien esta en cada room.
	participants map[string][]string
	// removed registra las expulsiones efectivas, que es lo que se verifica.
	removed []string
	// listFails / removeFails fuerzan errores del SFU.
	listFails   bool
	removeFails bool
	// authHeaders guarda los tokens recibidos, para comprobar que van firmados.
	authHeaders []string
}

func newFakeLiveKit(t *testing.T) *fakeLiveKit {
	t.Helper()

	f := &fakeLiveKit{participants: map[string][]string{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/twirp/livekit.RoomService/ListParticipants",
		func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.authHeaders = append(f.authHeaders, r.Header.Get("Authorization"))

			if f.listFails {
				twirpError(w, http.StatusInternalServerError, "internal", "el SFU se cayo")
				return
			}

			var req struct{ Room string }
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &req)

			type participant struct {
				Identity string `json:"identity"`
			}
			out := struct {
				Participants []participant `json:"participants"`
			}{}
			for _, identity := range f.participants[req.Room] {
				out.Participants = append(out.Participants, participant{Identity: identity})
			}
			writeTwirpJSON(w, out)
		})

	mux.HandleFunc("/twirp/livekit.RoomService/RemoveParticipant",
		func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()

			if f.removeFails {
				twirpError(w, http.StatusInternalServerError, "internal", "no se pudo expulsar")
				return
			}

			var req struct {
				Room     string `json:"room"`
				Identity string `json:"identity"`
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &req)

			f.removed = append(f.removed, req.Identity)
			writeTwirpJSON(w, struct{}{})
		})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeLiveKit) setParticipants(room string, identities ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.participants[room] = identities
}

func (f *fakeLiveKit) removedList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.removed))
	copy(out, f.removed)
	return out
}

func writeTwirpJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(payload)
}

func twirpError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"code":"` + code + `","msg":"` + msg + `"}`))
}

func (h *harness) revoke(hostID string, withKey bool) *response {
	h.t.Helper()

	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/internal/revoke",
		strings.NewReader(`{"host_id":"`+hostID+`"}`))
	if err != nil {
		h.t.Fatalf("no se pudo armar el request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if withKey {
		req.Header.Set("X-Internal-Api-Key", testInternalAPIKey)
	}

	resp, err := h.server.Client().Do(req)
	if err != nil {
		h.t.Fatalf("el request fallo: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	return &response{t: h.t, status: resp.StatusCode, body: raw}
}

type revokeBody struct {
	Room    string   `json:"room"`
	Checked int      `json:"checked"`
	Evicted int      `json:"evicted"`
	Removed []string `json:"removed"`
}

func TestRevokeEvictsOnlyThoseWhoLostPermission(t *testing.T) {
	h := newHarnessWithLiveKit(t)
	hostID := uuid.New()
	permitido := uuid.New()
	denegado := uuid.New()

	room := "listen:" + hostID.String()
	// El host tambien esta en su room, y no debe ser expulsado nunca.
	h.livekit.setParticipants(room, hostID.String(), permitido.String(), denegado.String())

	// core permite a uno y deniega al otro.
	h.core.permissionByListener = map[string]bool{
		permitido.String(): true,
		denegado.String():  false,
	}

	var body revokeBody
	h.revoke(hostID.String(), true).expectStatus(http.StatusOK).decode(&body)

	if body.Room != room {
		t.Errorf("room = %q, se esperaba %q", body.Room, room)
	}
	// Se revisan los dos oyentes, no el host.
	if body.Checked != 2 {
		t.Errorf("checked = %d, se esperaba 2 (el host no se revisa)", body.Checked)
	}
	if body.Evicted != 1 {
		t.Errorf("evicted = %d, se esperaba 1", body.Evicted)
	}

	removed := h.livekit.removedList()
	if len(removed) != 1 || removed[0] != denegado.String() {
		t.Fatalf("expulsados = %v, se esperaba solo %v", removed, denegado)
	}
	for _, identity := range removed {
		if identity == hostID.String() {
			t.Error("el host fue expulsado de su propio room")
		}
		if identity == permitido.String() {
			t.Error("se expulso a un oyente que si tenia permiso")
		}
	}
}

func TestRevokeEvictsEverybodyWhenSharingIsOff(t *testing.T) {
	h := newHarnessWithLiveKit(t)
	hostID := uuid.New()
	a, b := uuid.New(), uuid.New()

	room := "listen:" + hostID.String()
	h.livekit.setParticipants(room, hostID.String(), a.String(), b.String())
	// share_default = nobody: core deniega a todos.
	h.core.permissionStatus = http.StatusForbidden
	h.core.permissionBody = `{"allowed":false,"reason":"sharing_disabled"}`

	var body revokeBody
	h.revoke(hostID.String(), true).expectStatus(http.StatusOK).decode(&body)

	if body.Evicted != 2 {
		t.Errorf("evicted = %d, se esperaban los 2 oyentes", body.Evicted)
	}
	if len(h.livekit.removedList()) != 2 {
		t.Errorf("expulsados = %v, se esperaban 2", h.livekit.removedList())
	}
}

func TestRevokeOnEmptyRoomIsHarmless(t *testing.T) {
	h := newHarnessWithLiveKit(t)
	hostID := uuid.New()

	// Nadie escuchando: el caso mas comun, porque el aviso se manda en cada
	// cambio de permisos independientemente de si hay alguien en el room.
	var body revokeBody
	h.revoke(hostID.String(), true).expectStatus(http.StatusOK).decode(&body)

	if body.Checked != 0 || body.Evicted != 0 {
		t.Errorf("checked=%d evicted=%d, se esperaba 0/0", body.Checked, body.Evicted)
	}
}

func TestRevokeDoesNotEvictWhenCoreCannotBeAsked(t *testing.T) {
	h := newHarnessWithLiveKit(t)
	hostID := uuid.New()
	oyente := uuid.New()

	h.livekit.setParticipants("listen:"+hostID.String(), hostID.String(), oyente.String())
	// core no puede responder. Aca fallar cerrado seria echar gente por un
	// problema de red: el permiso se revalida igual en el proximo join.
	h.core.permissionStatus = http.StatusInternalServerError
	h.core.permissionBody = `{"error":{"code":"internal_error"}}`

	var body revokeBody
	h.revoke(hostID.String(), true).expectStatus(http.StatusOK).decode(&body)

	if body.Evicted != 0 {
		t.Errorf("evicted = %d, no se debe expulsar sin poder confirmar", body.Evicted)
	}
	if len(h.livekit.removedList()) != 0 {
		t.Errorf("se expulso a alguien sin confirmar el permiso: %v", h.livekit.removedList())
	}
}

func TestRevokeReportsWhenLiveKitIsDown(t *testing.T) {
	h := newHarnessWithLiveKit(t)
	h.livekit.listFails = true

	h.revoke(uuid.NewString(), true).expectStatus(http.StatusBadGateway)
}

func TestRevokeSurvivesFailedEviction(t *testing.T) {
	h := newHarnessWithLiveKit(t)
	hostID := uuid.New()
	a, b := uuid.New(), uuid.New()

	h.livekit.setParticipants("listen:"+hostID.String(), a.String(), b.String())
	h.core.permissionStatus = http.StatusForbidden
	h.core.permissionBody = `{"allowed":false,"reason":"sharing_disabled"}`
	h.livekit.removeFails = true

	// El SFU rechaza las expulsiones: se reporta 0 expulsados en vez de fingir
	// exito o cortar todo el proceso.
	var body revokeBody
	h.revoke(hostID.String(), true).expectStatus(http.StatusOK).decode(&body)

	if body.Evicted != 0 {
		t.Errorf("evicted = %d, se esperaba 0 porque el SFU fallo", body.Evicted)
	}
	if body.Checked != 2 {
		t.Errorf("checked = %d, se esperaba 2: se intento con los dos", body.Checked)
	}
}

func TestRevokeIgnoresNonUUIDIdentities(t *testing.T) {
	h := newHarnessWithLiveKit(t)
	hostID := uuid.New()

	// Una identidad que no emitio este servicio: no se toca, y no rompe el
	// procesamiento del resto.
	oyente := uuid.New()
	h.livekit.setParticipants("listen:"+hostID.String(), "bot-de-grabacion", oyente.String())
	h.core.permissionStatus = http.StatusForbidden
	h.core.permissionBody = `{"allowed":false,"reason":"sharing_disabled"}`

	var body revokeBody
	h.revoke(hostID.String(), true).expectStatus(http.StatusOK).decode(&body)

	if body.Checked != 1 {
		t.Errorf("checked = %d, se esperaba 1 (la identidad rara se ignora)", body.Checked)
	}
	removed := h.livekit.removedList()
	for _, identity := range removed {
		if identity == "bot-de-grabacion" {
			t.Error("se expulso una identidad que no emitimos nosotros")
		}
	}
}

func TestRevokeRequiresInternalAPIKey(t *testing.T) {
	h := newHarnessWithLiveKit(t)

	// Sin la clave compartida cualquiera podria echar gente de los rooms.
	h.revoke(uuid.NewString(), false).expectStatus(http.StatusUnauthorized)
}

func TestRevokeRejectsBadHostID(t *testing.T) {
	h := newHarnessWithLiveKit(t)

	h.revoke("no-es-uuid", true).expectStatus(http.StatusBadRequest)
}

func TestRevokeSignsCallsToLiveKit(t *testing.T) {
	h := newHarnessWithLiveKit(t)
	hostID := uuid.New()
	h.livekit.setParticipants("listen:"+hostID.String(), hostID.String())

	h.revoke(hostID.String(), true).expectStatus(http.StatusOK)

	h.livekit.mu.Lock()
	headers := h.livekit.authHeaders
	h.livekit.mu.Unlock()

	if len(headers) == 0 {
		t.Fatal("el SFU no recibio ninguna llamada")
	}
	// El SFU rechaza cualquier llamada sin un token de admin firmado.
	if !strings.HasPrefix(headers[0], "Bearer ey") {
		t.Errorf("Authorization = %q, se esperaba un JWT", headers[0])
	}
}

func TestRevokeDisabledWithoutLiveKitURL(t *testing.T) {
	// Sin LIVEKIT_API_URL no hay forma de expulsar: se responde 501 para que
	// core lo registre como problema de despliegue y no reintente.
	h := newHarness(t)

	h.revoke(uuid.NewString(), true).expectStatus(http.StatusNotImplemented)
}
