package httpserver

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/soundvibe/media/signaling/internal/rooms"
)

// subtleCompare existe solo para no importar crypto/subtle en dos archivos.
func subtleCompare(a, b []byte) int { return subtle.ConstantTimeCompare(a, b) }

// revokeRequest es el body de POST /internal/revoke.
//
// Solo lleva el host: quien se tenga que ir lo decide core, no este servicio.
type revokeRequest struct {
	HostID string `json:"host_id"`
}

type revokeResponse struct {
	Room string `json:"room"`
	// Checked es cuanta gente habia en el room, Evicted cuantos perdieron el
	// permiso y se echaron.
	Checked int      `json:"checked"`
	Evicted int      `json:"evicted"`
	Removed []string `json:"removed"`
}

// revoke expulsa del room de un host a quien ya no tenga permiso de escucharlo.
//
// Lo llama soundvibe-core cuando cambia algo que afecta los permisos: el
// share_default del host, una excepcion, o una amistad que se rompe. El permiso
// se valida al entrar al room, asi que sin esto el que ya estaba adentro seguia
// escuchando indefinidamente.
//
// Diseño: en vez de que core mande a quien echar, aca se listan los presentes y
// se le vuelve a preguntar a core por cada uno. Asi la politica vive en un solo
// lugar y este endpoint sirve igual para cualquier motivo de revocacion, sin
// enterarse de cual fue.
func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	var in revokeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		fail(w, r, http.StatusBadRequest, "bad_request", "el body no es JSON valido")
		return
	}

	hostID, err := uuid.Parse(strings.TrimSpace(in.HostID))
	if err != nil {
		fail(w, r, http.StatusBadRequest, "bad_request", "host_id debe ser un UUID valido")
		return
	}

	if !s.livekit.Enabled() {
		// Sin LIVEKIT_API_URL no hay forma de expulsar a nadie. Se responde 501
		// y no 500: no es una falla, es una capacidad no configurada, y core lo
		// registra sin reintentar.
		slog.WarnContext(r.Context(),
			"revocacion pedida pero LIVEKIT_API_URL no esta configurada",
			"host", hostID)
		fail(w, r, http.StatusNotImplemented, "revocation_disabled",
			"la expulsion de participantes no esta configurada en este despliegue")
		return
	}

	room := rooms.Name(hostID)

	identities, err := s.livekit.ListParticipantIdentities(r.Context(), room)
	if err != nil {
		slog.ErrorContext(r.Context(), "no se pudieron listar participantes",
			"room", room, "error", err)
		fail(w, r, http.StatusBadGateway, "livekit_unavailable",
			"no se pudo consultar el SFU")
		return
	}

	out := revokeResponse{Room: room, Removed: []string{}}

	for _, identity := range identities {
		listenerID, parseErr := uuid.Parse(identity)
		if parseErr != nil {
			// Toda identidad que emite este servicio es un UUID. Algo que no lo
			// sea no lo pusimos nosotros: se deja en paz y se registra.
			slog.WarnContext(r.Context(), "identidad inesperada en el room, se ignora",
				"room", room, "identity", identity)
			continue
		}
		// El host no se expulsa de su propio room.
		if listenerID == hostID {
			continue
		}
		out.Checked++

		allowed, reason, checkErr := s.core.CanListen(r.Context(), hostID, listenerID)
		if checkErr != nil {
			// No se pudo preguntar: NO se expulsa. Aca fallar cerrado seria
			// echar gente por un problema de red, y el permiso se revalida de
			// todos modos en el proximo join.
			slog.ErrorContext(r.Context(), "no se pudo revalidar el permiso, no se expulsa",
				"room", room, "listener", listenerID, "error", checkErr)
			continue
		}
		if allowed {
			continue
		}

		if removeErr := s.livekit.RemoveParticipant(r.Context(), room, identity); removeErr != nil {
			slog.ErrorContext(r.Context(), "no se pudo expulsar al participante",
				"room", room, "listener", listenerID, "error", removeErr)
			continue
		}

		slog.InfoContext(r.Context(), "participante expulsado por perdida de permiso",
			"room", room, "listener", listenerID, "reason", reason)
		out.Evicted++
		out.Removed = append(out.Removed, identity)
	}

	writeJSON(w, r, http.StatusOK, out)
}

// internalAPIKeyMiddleware protege /internal/* con la clave compartida con
// soundvibe-core. Misma comparacion en tiempo constante que del lado de core.
func internalAPIKeyMiddleware(expected string) func(http.Handler) http.Handler {
	expectedBytes := []byte(expected)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			provided := []byte(r.Header.Get("X-Internal-Api-Key"))
			if len(provided) != len(expectedBytes) ||
				subtleCompare(provided, expectedBytes) != 1 {
				fail(w, r, http.StatusUnauthorized, "unauthorized", "credenciales invalidas")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
