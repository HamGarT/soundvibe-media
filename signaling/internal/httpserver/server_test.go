package httpserver_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/soundvibe/media/signaling/internal/config"
	"github.com/soundvibe/media/signaling/internal/core"
	"github.com/soundvibe/media/signaling/internal/httpserver"
	"github.com/soundvibe/media/signaling/internal/livekit"
)

const (
	testInternalAPIKey = "clave-interna-de-test-0123456789"
	testAPISecret      = "un-secreto-de-livekit-para-tests-0123456789"
	testAccessToken    = "un-access-token-de-core-cualquiera"
)

// fakeCore imita a soundvibe-core. Se usa en vez del servicio real para poder
// simular lo que es dificil de provocar de verdad: core caido, respuestas
// corruptas, la API key mal configurada.
type fakeCore struct {
	server *httptest.Server

	userID   uuid.UUID
	username string

	// introspectStatus / permissionStatus permiten forzar respuestas concretas.
	introspectStatus int
	introspectBody   string
	permissionStatus int
	permissionBody   string

	// permissionByListener permite responder distinto segun el oyente, que es lo
	// que necesita la revocacion: en un mismo room unos siguen autorizados y
	// otros no. Si esta nil se usa permissionStatus/permissionBody.
	permissionByListener map[string]bool

	// requests cuenta las llamadas recibidas, para verificar que el permiso se
	// consulta siempre y no se saltea.
	introspectCalls int
	permissionCalls int

	lastHost     string
	lastListener string
}

func newFakeCore(t *testing.T) *fakeCore {
	t.Helper()

	f := &fakeCore{
		userID:           uuid.New(),
		username:         "ana",
		introspectStatus: http.StatusOK,
		permissionStatus: http.StatusOK,
		permissionBody:   `{"allowed":true,"reason":"default_all_friends"}`,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","database":"ok"}`))
	})
	mux.HandleFunc("/internal/introspect", func(w http.ResponseWriter, r *http.Request) {
		f.introspectCalls++
		if r.Header.Get("X-Internal-Api-Key") != testInternalAPIKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"no"}}`))
			return
		}
		w.WriteHeader(f.introspectStatus)
		if f.introspectBody != "" {
			_, _ = w.Write([]byte(f.introspectBody))
			return
		}
		_, _ = w.Write([]byte(`{"user_id":"` + f.userID.String() +
			`","username":"` + f.username + `","share_default":"all_friends"}`))
	})
	mux.HandleFunc("/internal/listening-permission", func(w http.ResponseWriter, r *http.Request) {
		f.permissionCalls++
		f.lastHost = r.URL.Query().Get("host")
		f.lastListener = r.URL.Query().Get("listener")
		if r.Header.Get("X-Internal-Api-Key") != testInternalAPIKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"no"}}`))
			return
		}

		if f.permissionByListener != nil {
			if allowed := f.permissionByListener[f.lastListener]; allowed {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"allowed":true,"reason":"default_all_friends"}`))
			} else {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"allowed":false,"reason":"explicitly_excluded"}`))
			}
			return
		}

		w.WriteHeader(f.permissionStatus)
		_, _ = w.Write([]byte(f.permissionBody))
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

type harness struct {
	t       *testing.T
	server  *httptest.Server
	core    *fakeCore
	livekit *fakeLiveKit
}

// newHarness arma el servicio sin API de LiveKit configurada, que es el caso
// donde la revocacion en vivo esta deshabilitada.
func newHarness(t *testing.T) *harness {
	t.Helper()

	fake := newFakeCore(t)
	return newHarnessWith(t, fake, fake.server.URL, nil)
}

// newHarnessWithLiveKit arma el servicio con un SFU falso, para los tests de
// revocacion.
func newHarnessWithLiveKit(t *testing.T) *harness {
	t.Helper()

	fake := newFakeCore(t)
	fakeLK := newFakeLiveKit(t)
	return newHarnessWith(t, fake, fake.server.URL, fakeLK)
}

func newHarnessWithCoreURL(t *testing.T, fake *fakeCore, coreURL string) *harness {
	t.Helper()
	return newHarnessWith(t, fake, coreURL, nil)
}

func newHarnessWith(t *testing.T, fake *fakeCore, coreURL string, fakeLK *fakeLiveKit) *harness {
	t.Helper()

	cfg := config.Config{
		Env:  "test",
		Port: "0",
		Core: config.CoreConfig{
			BaseURL: coreURL,
			APIKey:  testInternalAPIKey,
			Timeout: 2 * time.Second,
		},
		LiveKit: config.LiveKitConfig{
			URL:       "wss://livekit.soundvibe.test",
			APIKey:    "APItestkey",
			APISecret: testAPISecret,
			TokenTTL:  10 * time.Minute,
		},
	}

	if fakeLK != nil {
		cfg.LiveKit.APIURL = fakeLK.server.URL
	}

	server := httptest.NewServer(
		httpserver.New(cfg, core.New(cfg.Core), livekit.New(cfg.LiveKit)))
	t.Cleanup(server.Close)

	return &harness{t: t, server: server, core: fake, livekit: fakeLK}
}

type response struct {
	t      *testing.T
	status int
	body   []byte
}

func (r *response) expectStatus(want int) *response {
	r.t.Helper()
	if r.status != want {
		r.t.Fatalf("status = %d, se esperaba %d; body: %s", r.status, want, r.body)
	}
	return r
}

func (r *response) decode(dst any) *response {
	r.t.Helper()
	if err := json.Unmarshal(r.body, dst); err != nil {
		r.t.Fatalf("no se pudo decodificar (%s): %v", r.body, err)
	}
	return r
}

func (r *response) errorCode() string {
	r.t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	r.decode(&body)
	return body.Error.Code
}

func (h *harness) join(body any, withAuth bool) *response {
	h.t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("no se pudo serializar: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/rooms/join", reader)
	if err != nil {
		h.t.Fatalf("no se pudo armar el request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+testAccessToken)
	}

	resp, err := h.server.Client().Do(req)
	if err != nil {
		h.t.Fatalf("el request fallo: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("no se pudo leer la respuesta: %v", err)
	}
	return &response{t: h.t, status: resp.StatusCode, body: raw}
}

type tokenBody struct {
	LiveKitURL string `json:"livekit_url"`
	Token      string `json:"token"`
	Room       string `json:"room"`
	Identity   string `json:"identity"`
	Role       string `json:"role"`
}

func TestJoinAsListenerWhenAllowed(t *testing.T) {
	h := newHarness(t)
	hostID := uuid.New()

	var body tokenBody
	h.join(map[string]any{"host_id": hostID.String()}, true).
		expectStatus(http.StatusOK).decode(&body)

	if body.Token == "" {
		t.Error("se esperaba un token de LiveKit")
	}
	if body.Role != "listener" {
		t.Errorf("role = %q, se esperaba listener", body.Role)
	}
	if want := "listen:" + hostID.String(); body.Room != want {
		t.Errorf("room = %q, se esperaba %q", body.Room, want)
	}
	if body.Identity != h.core.userID.String() {
		t.Errorf("identity = %q, se esperaba %q", body.Identity, h.core.userID)
	}
	if body.LiveKitURL != "wss://livekit.soundvibe.test" {
		t.Errorf("livekit_url = %q", body.LiveKitURL)
	}

	// El permiso se consulta en la direccion correcta: host es el dueno del
	// room, listener es quien pide entrar.
	if h.core.lastHost != hostID.String() {
		t.Errorf("se consulto host=%q, se esperaba %q", h.core.lastHost, hostID)
	}
	if h.core.lastListener != h.core.userID.String() {
		t.Errorf("se consulto listener=%q, se esperaba %q", h.core.lastListener, h.core.userID)
	}
}

func TestJoinWithoutHostIDOpensOwnRoomAsHost(t *testing.T) {
	h := newHarness(t)

	var body tokenBody
	h.join(nil, true).expectStatus(http.StatusOK).decode(&body)

	if body.Role != "host" {
		t.Errorf("role = %q, se esperaba host", body.Role)
	}
	if want := "listen:" + h.core.userID.String(); body.Room != want {
		t.Errorf("room = %q, se esperaba el room propio %q", body.Room, want)
	}
}

func TestJoinAlwaysAsksCoreForPermission(t *testing.T) {
	h := newHarness(t)

	// Incluso abriendo el room propio: dejar la decision siempre del mismo lado
	// evita que esta condicion se desincronice de la de core.
	h.join(nil, true).expectStatus(http.StatusOK)

	if h.core.introspectCalls != 1 {
		t.Errorf("introspectCalls = %d, se esperaba 1", h.core.introspectCalls)
	}
	if h.core.permissionCalls != 1 {
		t.Errorf("permissionCalls = %d, se esperaba 1", h.core.permissionCalls)
	}
}

func TestJoinDeniedWhenCoreSaysNo(t *testing.T) {
	h := newHarness(t)
	h.core.permissionStatus = http.StatusForbidden
	h.core.permissionBody = `{"allowed":false,"reason":"explicitly_excluded"}`

	resp := h.join(map[string]any{"host_id": uuid.NewString()}, true).
		expectStatus(http.StatusForbidden)

	if code := resp.errorCode(); code != "listening_not_allowed" {
		t.Errorf("code = %q, se esperaba listening_not_allowed", code)
	}
	// Lo importante: no se emitio ningun token.
	if bytes := string(resp.body); contains(bytes, "token") {
		t.Errorf("una denegacion no debe traer token: %s", bytes)
	}
}

func TestJoinFailsClosedWhenCoreIsDown(t *testing.T) {
	fake := newFakeCore(t)
	// Se apunta a un puerto donde no hay nada escuchando: es exactamente lo que
	// pasa si el stack de core esta caido.
	h := newHarnessWithCoreURL(t, fake, "http://127.0.0.1:1")

	resp := h.join(map[string]any{"host_id": uuid.NewString()}, true).
		expectStatus(http.StatusServiceUnavailable)

	if code := resp.errorCode(); code != "core_unavailable" {
		t.Errorf("code = %q, se esperaba core_unavailable", code)
	}
	if contains(string(resp.body), "token") {
		t.Error("con core caido no se debe emitir ningun token")
	}
}

func TestJoinFailsClosedWhenCoreErrors(t *testing.T) {
	cases := []struct {
		name             string
		permissionStatus int
		permissionBody   string
	}{
		{"core devuelve 500", http.StatusInternalServerError, `{"error":{"code":"internal_error"}}`},
		{"core devuelve JSON corrupto", http.StatusOK, `{no es json`},
		{"core devuelve 200 con allowed=false", http.StatusOK, `{"allowed":false,"reason":"raro"}`},
		{"core devuelve un body vacio", http.StatusOK, ``},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.core.permissionStatus = tc.permissionStatus
			h.core.permissionBody = tc.permissionBody

			// Cualquier cosa que no sea un si explicito es un no.
			resp := h.join(map[string]any{"host_id": uuid.NewString()}, true)
			if resp.status == http.StatusOK {
				t.Errorf("se emitio un token con una respuesta ambigua de core: %s", resp.body)
			}
		})
	}
}

func TestJoinRejectsBadAccessToken(t *testing.T) {
	h := newHarness(t)
	h.core.introspectStatus = http.StatusUnauthorized
	h.core.introspectBody = `{"error":{"code":"invalid_access_token","message":"no"}}`

	resp := h.join(nil, true).expectStatus(http.StatusUnauthorized)
	if code := resp.errorCode(); code != "unauthorized" {
		t.Errorf("code = %q, se esperaba unauthorized", code)
	}

	// Un token invalido se corta antes de preguntar por permisos.
	if h.core.permissionCalls != 0 {
		t.Errorf("permissionCalls = %d, no deberia consultarse sin identidad", h.core.permissionCalls)
	}
}

func TestJoinDistinguishesBadAPIKeyFromBadUserToken(t *testing.T) {
	fake := newFakeCore(t)
	h := newHarnessWithCoreURL(t, fake, fake.server.URL)

	// core rechaza por la API key del servicio, no por el token del usuario.
	// Devolver 401 aca mandaria al usuario a reautenticarse cuando el problema
	// es de configuracion del servidor.
	fake.introspectStatus = http.StatusUnauthorized
	fake.introspectBody = `{"error":{"code":"unauthorized","message":"no"}}`

	resp := h.join(nil, true).expectStatus(http.StatusServiceUnavailable)
	if code := resp.errorCode(); code != "core_unavailable" {
		t.Errorf("code = %q, se esperaba core_unavailable", code)
	}
}

func TestJoinRequiresAuthHeader(t *testing.T) {
	h := newHarness(t)

	resp := h.join(map[string]any{"host_id": uuid.NewString()}, false).
		expectStatus(http.StatusUnauthorized)
	if code := resp.errorCode(); code != "unauthorized" {
		t.Errorf("code = %q, se esperaba unauthorized", code)
	}
	if h.core.introspectCalls != 0 {
		t.Error("sin header Authorization no hay que molestar a core")
	}
}

func TestJoinRejectsBadHostID(t *testing.T) {
	h := newHarness(t)

	h.join(map[string]any{"host_id": "no-es-uuid"}, true).
		expectStatus(http.StatusBadRequest)
}

func TestJoinRejectsBadJSON(t *testing.T) {
	h := newHarness(t)

	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/rooms/join",
		bytes.NewReader([]byte(`{no es json`)))
	if err != nil {
		t.Fatalf("no se pudo armar el request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAccessToken)

	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("el request fallo: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, se esperaba 400", resp.StatusCode)
	}
}

func TestHealth(t *testing.T) {
	h := newHarness(t)

	var body struct {
		Status string `json:"status"`
		Core   string `json:"core"`
	}
	resp, err := h.server.Client().Get(h.server.URL + "/health")
	if err != nil {
		t.Fatalf("el request fallo: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, se esperaba 200", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body ilegible (%s): %v", raw, err)
	}
	if body.Status != "ok" || body.Core != "ok" {
		t.Errorf("health = %+v, se esperaba todo ok", body)
	}
}

func TestHealthReportsDegradedWhenCoreIsDown(t *testing.T) {
	fake := newFakeCore(t)
	h := newHarnessWithCoreURL(t, fake, "http://127.0.0.1:1")

	// Sigue respondiendo 200: este proceso esta vivo, y marcarlo como no sano
	// haria que el orquestador lo reiniciara sin arreglar nada.
	resp, err := h.server.Client().Get(h.server.URL + "/health")
	if err != nil {
		t.Fatalf("el request fallo: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, se esperaba 200 aun con core caido", resp.StatusCode)
	}
	if !contains(string(raw), "degraded") || !contains(string(raw), "unreachable") {
		t.Errorf("health = %s, se esperaba degraded/unreachable", raw)
	}
}

func TestUnknownRouteIsJSON404(t *testing.T) {
	h := newHarness(t)

	resp, err := h.server.Client().Get(h.server.URL + "/no-existe")
	if err != nil {
		t.Fatalf("el request fallo: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, se esperaba 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !contains(ct, "application/json") {
		t.Errorf("content-type = %q, se esperaba JSON", ct)
	}
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
