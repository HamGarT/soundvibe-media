package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/soundvibe/media/signaling/internal/core"
	"github.com/soundvibe/media/signaling/internal/rooms"
)

// joinRequest es el body de POST /rooms/join.
//
// host_id es el dueno del room al que se quiere entrar a escuchar. Ya no se
// admite vacio: transmitir no se pide por aca, se abre el WebSocket de
// GET /rooms/broadcast.
type joinRequest struct {
	HostID string `json:"host_id"`
}

// join es el unico endpoint que importa de este servicio.
//
// Secuencia: autenticar contra core -> preguntar el permiso a core -> firmar el
// token de LiveKit. Nunca al reves, y nunca salteando el paso del medio.
func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	accessToken, ok := bearerToken(r)
	if !ok {
		fail(w, r, http.StatusUnauthorized, "unauthorized",
			"falta el access token de soundvibe-core")
		return
	}

	var in joinRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			fail(w, r, http.StatusBadRequest, "bad_request", "el body no es JSON valido")
			return
		}
	}

	identity, err := s.core.Introspect(r.Context(), accessToken)
	if err != nil {
		s.failCoreError(w, r, err, "no se pudo autenticar el access token")
		return
	}

	raw := strings.TrimSpace(in.HostID)
	if raw == "" {
		fail(w, r, http.StatusBadRequest, "bad_request",
			"host_id es obligatorio: para transmitir se usa GET /rooms/broadcast")
		return
	}
	hostID, parseErr := uuid.Parse(raw)
	if parseErr != nil {
		fail(w, r, http.StatusBadRequest, "bad_request", "host_id debe ser un UUID valido")
		return
	}

	// Entrar al room propio ya no tiene sentido, y ademas rompe: desde que el
	// audio lo publica el relay, este servicio ya esta dentro del room con la
	// identidad del host. Un segundo participante con la misma identidad hace que
	// LiveKit eche a uno de los dos, y el que se caiga puede ser la transmision.
	if hostID == identity.UserID {
		fail(w, r, http.StatusConflict, "own_room",
			"no puedes entrar a tu propio room: tu audio lo publica el servidor mientras transmites")
		return
	}

	// El unico publicador del room es el relay. Todo token que se firma aca es de
	// oyente, sin permiso de publicar.
	role := rooms.RoleListener

	// Se consulta el permiso incluso cuando el host es uno mismo: core responde
	// `self` en ese caso, y dejar la decision siempre del mismo lado evita que
	// esta condicion se desincronice de la de core.
	allowed, reason, err := s.core.CanListen(r.Context(), hostID, identity.UserID)
	if err != nil {
		s.failCoreError(w, r, err, "no se pudo verificar el permiso de escucha")
		return
	}
	if !allowed {
		slog.InfoContext(r.Context(), "join denegado",
			"listener", identity.UserID, "host", hostID, "reason", reason)
		fail(w, r, http.StatusForbidden, "listening_not_allowed",
			"no tienes permiso para escuchar la actividad de ese usuario")
		return
	}

	token, err := s.minter.Mint(hostID, identity.UserID, identity.Username, role)
	if err != nil {
		slog.ErrorContext(r.Context(), "no se pudo firmar el token de LiveKit", "error", err)
		fail(w, r, http.StatusInternalServerError, "internal_error", "error interno del servidor")
		return
	}

	// Recien aca, con el permiso ya concedido: pedirle audio antes seria avisarle
	// al host que alguien intento escucharlo aunque no tuviera permiso.
	//
	// Y antes de responder, no despues: el oyente se conecta al SFU apenas recibe
	// este token, asi que el audio tiene que estar ya en camino cuando llegue.
	s.audience.Requested(r.Context(), hostID, identity.UserID)

	slog.InfoContext(r.Context(), "join permitido",
		"listener", identity.UserID, "host", hostID, "role", role, "reason", reason)
	writeJSON(w, r, http.StatusOK, token)
}

// failCoreError traduce un error del cliente de core al status correcto.
//
// La distincion importa: un token de usuario invalido es un 401 que el cliente
// arregla reautenticandose, mientras que core caido es un 503 que el cliente no
// puede arreglar y que no debe empujarlo a borrar su sesion.
func (s *Server) failCoreError(w http.ResponseWriter, r *http.Request, err error, context string) {
	switch {
	case errors.Is(err, core.ErrUnauthenticated):
		fail(w, r, http.StatusUnauthorized, "unauthorized",
			"el access token es invalido o expiro")

	case errors.Is(err, core.ErrUnavailable):
		// Fallar cerrado: sin respuesta de core no se entrega ningun token.
		slog.ErrorContext(r.Context(), context, "error", err)
		fail(w, r, http.StatusServiceUnavailable, "core_unavailable",
			"el servicio de usuarios no esta disponible, intenta de nuevo")

	default:
		slog.ErrorContext(r.Context(), context, "error", err)
		fail(w, r, http.StatusInternalServerError, "internal_error", "error interno del servidor")
	}
}

type healthResponse struct {
	Status string `json:"status"`
	Core   string `json:"core"`
}

// health reporta el estado propio y el de core. Se responde 200 aunque core
// este caido: este proceso sigue vivo, y marcarlo como no sano haria que el
// orquestador lo reiniciara sin arreglar nada.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	body := healthResponse{Status: "ok", Core: "ok"}
	if err := s.core.Health(ctx); err != nil {
		body.Status = "degraded"
		body.Core = "unreachable"
	}
	writeJSON(w, r, http.StatusOK, body)
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}
