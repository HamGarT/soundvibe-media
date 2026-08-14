package httpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"

	"github.com/soundvibe/media/signaling/internal/presence"
)

// maxPresenceBytes acota lo que se acepta por mensaje.
//
// Un anuncio son cuatro campos de texto y dos numeros. El limite deja lugar de
// sobra para titulos largos sin que un cliente pueda mandar megabytes por
// mensaje.
const maxPresenceBytes = 4 << 10

// presenceUpdate es lo que manda el host por el socket: que esta sonando ahora.
//
// Se acepta con campos vacios a proposito. La biblioteca sale de los tags de
// archivos locales, que vienen a medias mas seguido de lo que uno querria, y un
// titulo sin artista sigue siendo mejor que dejar al host sin nada que mostrar.
type presenceUpdate struct {
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Album      string `json:"album"`
	DurationMs int64  `json:"duration_ms"`
	PositionMs int64  `json:"position_ms"`
}

// presence mantiene abierto el canal por el que un host dice "estoy escuchando
// esto".
//
// Es un socket **separado** del de audio, y esa es la decision de fondo: el host
// publica audio bajo demanda, recien cuando alguien toca tune in, porque
// transmitir sin oyentes le gasta bateria y datos para nadie. Si la presencia
// viajara por el socket de audio, un host que escucha solo no aparecerian en
// ninguna parte y nadie podria tunear con el nunca. Este canal es liviano — un
// JSON cada [presence.Heartbeat] — y se mantiene abierto mientras el telefono
// reproduce.
//
// No se consulta el permiso de escucha aca: el host anuncia su propia actividad,
// y quien decide si alguien mas puede verla es /rooms/active, del lado del que
// mira. Un solo lugar donde se pregunta, que es el mismo por el que pasa el
// audio.
func (s *Server) presence(w http.ResponseWriter, r *http.Request) {
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

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		// Accept ya respondio el error HTTP que corresponda.
		slog.InfoContext(r.Context(), "no se pudo abrir el WebSocket de presencia",
			"host", identity.UserID, "error", err)
		return
	}
	conn.SetReadLimit(maxPresenceBytes)

	slog.InfoContext(r.Context(), "presencia iniciada",
		"host", identity.UserID, "username", identity.Username)

	// La baja limpia. La sucia — el telefono que pierde la red sin cerrar — la
	// cubre el TTL del store, porque en ese caso este defer no corre nunca.
	defer func() {
		s.presenceStore.Clear(identity.UserID)
		slog.InfoContext(r.Context(), "presencia terminada", "host", identity.UserID)
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// El canal de vuelta: por aca se le dice al host que empiece o deje de
	// transmitir. Es lo que permite que no publique audio hasta que haya alguien
	// escuchando, en vez de subir 128 kbps al vacio por las dudas.
	commands, detach := s.presenceStore.Attach(identity.UserID)
	defer detach()

	// Escribir va en su propia goroutine porque leer bloquea: el bucle de abajo
	// se queda esperando el proximo anuncio del telefono, que puede tardar
	// quince segundos, y una orden no puede esperar a eso para salir.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case cmd, open := <-commands:
				if !open {
					return
				}
				payload := `{"type":"` + string(cmd) + `"}`
				if err := conn.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
					// El socket se murio. Cancelar despierta al lector, que es
					// quien hace la limpieza.
					cancel()
					return
				}
				slog.InfoContext(ctx, "orden enviada al host",
					"host", identity.UserID, "orden", cmd)
			}
		}
	}()
	for {
		messageType, data, readErr := conn.Read(ctx)
		if readErr != nil {
			// Que el host cierre la app o se quede sin red es el final normal de
			// una sesion de escucha, no un error del servicio.
			if websocket.CloseStatus(readErr) == -1 && ctx.Err() == nil {
				slog.InfoContext(ctx, "la presencia se corto",
					"host", identity.UserID, "error", readErr)
			}
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		}

		if messageType != websocket.MessageText {
			// Los anuncios son JSON. Un mensaje binario es un cliente mal
			// implementado — o el de audio equivocandose de socket.
			continue
		}

		var update presenceUpdate
		if err := json.Unmarshal(data, &update); err != nil {
			// Un anuncio ilegible se descarta sin cerrar el socket: el proximo
			// latido probablemente venga bien, y tirar la conexion dejaria al
			// host invisible por un error de una sola cancion.
			slog.WarnContext(ctx, "anuncio de presencia ilegible",
				"host", identity.UserID, "error", err)
			continue
		}

		// Un anuncio sin titulo no tiene nada que mostrar. Se trata como "segui
		// contandome como activo" y se refresca lo que ya habia, en vez de
		// pisarlo con campos vacios.
		if update.Title == "" {
			if previous, ok := s.presenceStore.Get(identity.UserID); ok {
				s.presenceStore.Announce(identity.UserID, identity.Username, previous.Track)
			}
			continue
		}

		s.presenceStore.Announce(identity.UserID, identity.Username, presence.Track{
			Title:      update.Title,
			Artist:     update.Artist,
			Album:      update.Album,
			DurationMs: update.DurationMs,
			PositionMs: update.PositionMs,
		})
	}
}
