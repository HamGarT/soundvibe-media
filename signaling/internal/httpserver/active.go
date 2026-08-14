package httpserver

import (
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/soundvibe/media/signaling/internal/presence"
	"github.com/soundvibe/media/signaling/internal/rooms"
)

// activeHost es un amigo escuchando algo ahora mismo, al que el usuario puede
// entrar a acompaniar.
type activeHost struct {
	HostID      uuid.UUID `json:"host_id"`
	Username    string    `json:"username"`
	DisplayName *string   `json:"display_name"`
	AvatarURL   *string   `json:"avatar_url"`
	// Room es el nombre que hay que pasarle al SDK de LiveKit despues de pedir
	// el token en /rooms/join. Se manda ya resuelto para que el cliente no tenga
	// que replicar la convencion de nombres.
	Room string `json:"room"`

	// Que esta sonando. Van vacios cuando el host esta transmitiendo audio pero
	// todavia no anuncio nada por el socket de presencia; la pantalla lo muestra
	// activo igual, porque estar activo es lo que habilita el boton.
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Album      string `json:"album"`
	DurationMs int64  `json:"duration_ms"`
	// PositionMs es donde iba el host en su ultimo anuncio, no ahora mismo:
	// entre latidos pasan segundos. Sirve para pintar una barra aproximada, no
	// para sincronizar nada.
	PositionMs int64 `json:"position_ms"`
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

	// Quien esta activo sale de la capa de presencia, no de quien esta
	// transmitiendo: el audio arranca recien cuando alguien toca tune in, asi que
	// mirar solo el relay daria por inactivo a todo el que todavia escucha solo
	// — que es justamente la gente con la que uno querria tunear.
	//
	// Se unen igual los que si estan transmitiendo. En la practica ya anunciaron
	// presencia, pero un host cuyo socket de presencia se cayo mientras el audio
	// sigue fluyendo esta demostrablemente activo, y esconderlo dejaria a sus
	// oyentes sin forma de volver a entrar.
	live := make(map[uuid.UUID]presence.Track)
	for _, entry := range s.presenceStore.Active() {
		live[entry.HostID] = entry.Track
	}
	broadcasting := s.relay.Broadcasting()
	for _, hostID := range broadcasting {
		if _, announced := live[hostID]; !announced {
			live[hostID] = presence.Track{}
		}
	}

	// El conjunto de hosts activos es chico, asi que se le pregunta a core por
	// los permisos de unos pocos y no de toda la lista de amigos.
	if len(live) == 0 {
		writeJSON(w, r, http.StatusOK, activeResponse{Hosts: []activeHost{}})
		return
	}

	friends, err := s.core.Friends(r.Context(), accessToken)
	if err != nil {
		s.failCoreError(w, r, err, "no se pudo obtener la lista de amigos")
		return
	}

	candidates := make([]uuid.UUID, 0, len(friends))
	byID := make(map[uuid.UUID]activeHost, len(friends))
	for _, friend := range friends {
		track, isLive := live[friend.UserID]
		if !isLive {
			continue
		}
		candidates = append(candidates, friend.UserID)
		byID[friend.UserID] = activeHost{
			HostID:      friend.UserID,
			Username:    friend.Username,
			DisplayName: friend.DisplayName,
			AvatarURL:   friend.AvatarURL,
			Room:        rooms.Name(friend.UserID),
			Title:       track.Title,
			Artist:      track.Artist,
			Album:       track.Album,
			DurationMs:  track.DurationMs,
			PositionMs:  track.PositionMs,
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

	// Info, no Debug: el nivel por defecto de slog es Info, asi que en Debug esta
	// linea no se ve nunca — y es justo la que dice por que la pantalla de amigos
	// salio vacia (nadie transmite / no son amigos / permiso denegado).
	slog.InfoContext(r.Context(), "amigos activos",
		"listener", identity.UserID, "con_presencia", s.presenceStore.Count(),
		"transmitiendo", len(broadcasting), "amigos_activos", len(candidates),
		"permitidos", len(hosts))

	writeJSON(w, r, http.StatusOK, activeResponse{Hosts: hosts})
}
