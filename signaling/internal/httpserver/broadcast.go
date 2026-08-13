package httpserver

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"

	"github.com/soundvibe/media/signaling/internal/relay"
)

// maxFrameBytes acota lo que se acepta por mensaje.
//
// Un frame de Opus de 20 ms a 128 kbps son ~320 bytes; el limite deja mucho aire
// para picos del codificador sin que un cliente pueda mandar megabytes por
// mensaje y hacer que el proceso reserve memoria a pedido.
const maxFrameBytes = 8 << 10

// broadcast recibe el audio del host por WebSocket y lo publica en su room.
//
// Cada mensaje binario es **un frame de Opus ya codificado por el telefono**,
// que se reenvia tal cual. El telefono no habla WebRTC en ningun momento: si lo
// hiciera tendria que abrir el microfono, porque el SDK de Android solo publica
// audio a traves de un AudioDeviceModule y el suyo siempre graba. Ver el
// comentario del paquete relay.
//
// La autenticacion pasa antes del upgrade, para poder responder un 401 normal
// en vez de abrir el socket y cerrarlo enseguida.
//
// No se consulta el permiso de escucha: el host esta publicando su propia
// actividad, y quien decide si alguien mas puede oirla es /rooms/join del lado
// del oyente.
func (s *Server) broadcast(w http.ResponseWriter, r *http.Request) {
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
		slog.InfoContext(r.Context(), "no se pudo abrir el WebSocket de transmision",
			"host", identity.UserID, "error", err)
		return
	}
	// Opus ya viene comprimido: dejar que el WebSocket lo comprima otra vez solo
	// gasta CPU en ambas puntas para no bajar nada.
	conn.SetReadLimit(maxFrameBytes)

	session, err := s.relay.Start(identity.UserID, identity.Username)
	if err != nil {
		slog.ErrorContext(r.Context(), "no se pudo abrir la transmision",
			"host", identity.UserID, "error", err)
		_ = conn.Close(websocket.StatusInternalError, "no se pudo abrir la transmision")
		return
	}

	slog.InfoContext(r.Context(), "transmision iniciada",
		"host", identity.UserID, "username", identity.Username)

	defer func() {
		s.relay.Stop(identity.UserID)
		slog.InfoContext(r.Context(), "transmision terminada",
			"host", identity.UserID, "frames", session.Frames())
	}()

	ctx := r.Context()
	for {
		messageType, data, readErr := conn.Read(ctx)
		if readErr != nil {
			// Que el host cierre o se le caiga la red es el final normal de una
			// transmision, no un error del servicio.
			if websocket.CloseStatus(readErr) == -1 && ctx.Err() == nil {
				slog.InfoContext(ctx, "la transmision se corto",
					"host", identity.UserID, "error", readErr)
			}
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		}

		if messageType != websocket.MessageBinary {
			// Los frames son binarios. Un mensaje de texto es un cliente mal
			// implementado, no algo que valga la pena adivinar.
			continue
		}

		// Cada ~10 s de audio. Es la unica prueba de que el telefono esta mandando
		// algo: "transmision iniciada" solo dice que el socket se abrio, y un
		// stream mudo se ve exactamente igual que uno que funciona.
		if frames := session.Frames(); frames > 0 && frames%500 == 0 {
			slog.InfoContext(ctx, "recibiendo audio",
				"host", identity.UserID, "frames", frames)
		}

		if writeErr := session.WriteFrame(data); writeErr != nil {
			if errors.Is(writeErr, relay.ErrSessionClosed) {
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			slog.WarnContext(ctx, "no se pudo reenviar un frame",
				"host", identity.UserID, "error", writeErr)
		}
	}
}
