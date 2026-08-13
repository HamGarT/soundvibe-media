package httpserver

import (
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/soundvibe/media/signaling/internal/rooms"
)

// activeHost es un amigo transmitiendo ahora mismo, al que el usuario puede
// entrar a escuchar.
type activeHost struct {
	HostID      uuid.UUID `json:"host_id"`
	Username    string    `json:"username"`
	DisplayName *string   `json:"display_name"`
	AvatarURL   *string   `json:"avatar_url"`
	// Room es el nombre que hay que pasarle al SDK de LiveKit despues de pedir
	// el token en /rooms/join. Se manda ya resuelto para que el cliente no tenga
	// que replicar la convencion de nombres.
	Room string `json:"room"`
}

type activeResponse struct {
	Hosts []activeHost `json:"hosts"`
}

// active responde que amigos estan transmitiendo y el usuario tiene permiso de
// escuchar. Es lo que pinta la pantalla de amigos.
//
// El orden importa y es el mismo que en /rooms/join: identificar, luego cruzar
// con quien transmite, y solo entonces preguntar permisos. La metadata pasa por
// el MISMO filtro que el audio — mostrar "fulano esta escuchando X" a alguien a
// quien fulano excluyo seria filtrar su actividad por la UI mientras se bloquea
// con cuidado su audio.
//
// La lista de amigos se pide a core con el token del propio usuario, asi que
// este servicio nunca ve amistades de terceros.
func (s *Server) active(w http.ResponseWriter, r *http.Request) {
	accessToken, ok := bearerToken(r)
	if !ok {
		fail(w, r, http.StatusUnauthorized, "unauthorized",
			"falta el access token de soundvibe-core")
		return
	}

	identity, err := s.core.Introspect(r.Context(), accessToken)
	if err != nil {
		s.failCoreError(w, r, err, "no se pudo autenticar el access token")
		return
	}

	// Se cruza primero contra quien transmite: el conjunto de hosts activos es
	// chico, asi que se le pregunta a core por permisos de unos pocos y no de
	// toda la lista de amigos.
	broadcasting := s.relay.Broadcasting()
	if len(broadcasting) == 0 {
		writeJSON(w, r, http.StatusOK, activeResponse{Hosts: []activeHost{}})
		return
	}

	friends, err := s.core.Friends(r.Context(), accessToken)
	if err != nil {
		s.failCoreError(w, r, err, "no se pudo obtener la lista de amigos")
		return
	}

	live := make(map[uuid.UUID]struct{}, len(broadcasting))
	for _, hostID := range broadcasting {
		live[hostID] = struct{}{}
	}

	candidates := make([]uuid.UUID, 0, len(friends))
	byID := make(map[uuid.UUID]activeHost, len(friends))
	for _, friend := range friends {
		if _, isLive := live[friend.UserID]; !isLive {
			continue
		}
		candidates = append(candidates, friend.UserID)
		byID[friend.UserID] = activeHost{
			HostID:      friend.UserID,
			Username:    friend.Username,
			DisplayName: friend.DisplayName,
			AvatarURL:   friend.AvatarURL,
			Room:        rooms.Name(friend.UserID),
		}
	}

	if len(candidates) == 0 {
		writeJSON(w, r, http.StatusOK, activeResponse{Hosts: []activeHost{}})
		return
	}

	allowed, err := s.core.CanListenBatch(r.Context(), identity.UserID, candidates)
	if err != nil {
		// Fallar cerrado: sin respuesta de core no se revela quien esta activo.
		s.failCoreError(w, r, err, "no se pudieron verificar los permisos de escucha")
		return
	}

	hosts := make([]activeHost, 0, len(allowed))
	for _, hostID := range allowed {
		if host, ok := byID[hostID]; ok {
			hosts = append(hosts, host)
		}
	}

	slog.DebugContext(r.Context(), "amigos activos",
		"listener", identity.UserID, "transmitiendo", len(broadcasting),
		"amigos_activos", len(candidates), "permitidos", len(hosts))

	writeJSON(w, r, http.StatusOK, activeResponse{Hosts: hosts})
}
