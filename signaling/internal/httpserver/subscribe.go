package httpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

const (
	// subscribeDebounce agrupa los cambios que llegan pegados.
	//
	// Un barrido que vence a tres hosts a la vez, o dos amigos que cambian de
	// cancion en el mismo segundo, son un solo recalculo. Sin esto cada cambio
	// costaria una consulta de amigos y otra de permisos a core, por cada
	// suscriptor.
	subscribeDebounce = 300 * time.Millisecond

	// subscribeFloor es lo minimo que se espera entre dos envios al mismo
	// cliente, pase lo que pase.
	//
	// Es el tope de trabajo que un host puede provocarle a core: por mas que
	// alguien haga skip veinte veces seguidas, sus amigos recalculan una vez cada
	// dos segundos y no veinte.
	subscribeFloor = 2 * time.Second

	// subscribeKeepalive es cada cuanto se manda algo si no cambio nada.
	//
	// No es por la presencia sino por la conexion: los proxies cortan los
	// WebSocket ociosos, y este puede estarlo por horas si los amigos del usuario
	// no estan escuchando nada. Tambien es lo que hace que el servidor note un
	// cliente muerto, porque el envio falla.
	subscribeKeepalive = 30 * time.Second
)

// subscribe empuja la lista de amigos activos cada vez que cambia.
//
// Es el mismo contenido que devuelve `GET /rooms/active` — y de hecho lo calcula
// la misma funcion, [Server.activeHostsFor], a proposito: son dos formas de
// entregar una respuesta, no dos respuestas. Si se duplicara la logica, una de
// las dos copias podria olvidarse de preguntar por los permisos, que es lo que
// la decision 4 del plan no quiere que pueda pasar.
//
// El servidor manda; el cliente no dice nada. No hay nada que un oyente pueda
// pedir por aca: que ve lo decide su lista de amigos y los permisos de cada
// host, nunca lo que el cliente afirme sobre si mismo.
func (s *Server) subscribe(w http.ResponseWriter, r *http.Request) {
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
		slog.InfoContext(r.Context(), "no se pudo abrir el WebSocket de suscripcion",
			"listener", identity.UserID, "error", err)
		return
	}

	// El cliente no habla, asi que cualquier cosa que mande es un error de
	// implementacion suya. El limite es simbolico: no se lee nada.
	conn.SetReadLimit(512)

	changes, unwatch := s.presenceStore.Watch()
	defer unwatch()

	slog.InfoContext(r.Context(), "suscripcion iniciada", "listener", identity.UserID)
	defer slog.InfoContext(r.Context(), "suscripcion terminada", "listener", identity.UserID)

	ctx := r.Context()

	// Suscribirse antes de mandar el primer estado, no despues: al reves hay una
	// ventana entre calcular la foto y empezar a escuchar, y un cambio que caiga
	// justo ahi no lo trae ninguno de los dos caminos. El cliente se quedaria con
	// una lista vieja hasta el proximo cambio, que puede no llegar nunca.
	lastSent, ok := s.pushActiveHosts(ctx, conn, identity.UserID, accessToken, "")
	if !ok {
		return
	}

	keepalive := time.NewTicker(subscribeKeepalive)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return

		case _, open := <-changes:
			if !open {
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return
			}

			// Ventana corta para que los cambios pegados entren en un solo envio.
			select {
			case <-ctx.Done():
				return
			case <-time.After(subscribeDebounce):
			}
			drain(changes)

			lastSent, ok = s.pushActiveHosts(ctx, conn, identity.UserID, accessToken, lastSent)
			if !ok {
				return
			}

			// El piso se aplica despues de mandar, no antes: el primer cambio sale
			// enseguida y los siguientes esperan, que es al reves de como se
			// sentiria si se durmiera primero.
			select {
			case <-ctx.Done():
				return
			case <-time.After(subscribeFloor):
			}
			drain(changes)

		case <-keepalive.C:
			if err := conn.Ping(ctx); err != nil {
				// El cliente se fue sin cerrar. Normal en un telefono.
				return
			}
		}
	}
}

// drain se queda con las seniales acumuladas mientras se esperaba.
//
// Ya se van a ver reflejadas en el recalculo que viene, asi que dejarlas en el
// canal solo provocaria una vuelta extra que no cambia nada.
func drain(changes <-chan struct{}) {
	for {
		select {
		case <-changes:
		default:
			return
		}
	}
}

// pushActiveHosts recalcula y manda, si cambio respecto de `previous`.
//
// Devuelve lo que quedo enviado y si la conexion sigue viva.
func (s *Server) pushActiveHosts(
	ctx context.Context,
	conn *websocket.Conn,
	listenerID uuid.UUID,
	accessToken, previous string,
) (string, bool) {
	hosts, err := s.activeHostsFor(ctx, listenerID, accessToken)
	if err != nil {
		// Fallar cerrado, igual que el endpoint HTTP: sin respuesta de core no se
		// revela quien esta activo. Se deja lo ultimo que el cliente ya tenia en
		// vez de mandarle una lista vacia, porque "no pude preguntar" no es lo
		// mismo que "no hay nadie" y la diferencia se ve en pantalla.
		slog.WarnContext(ctx, "no se pudieron resolver los amigos activos",
			"listener", listenerID, "error", err)
		return previous, true
	}

	payload, err := json.Marshal(activeResponse{Hosts: hosts})
	if err != nil {
		slog.ErrorContext(ctx, "no se pudo serializar los amigos activos",
			"listener", listenerID, "error", err)
		return previous, true
	}

	// Nada que decir si el resultado es identico al anterior. Pasa seguido: el
	// cambio que desperto al suscriptor puede ser de un host que este oyente no
	// tiene permitido ver, y despertarlo para mandarle la misma lista le costaria
	// radio al telefono para nada.
	if string(payload) == previous {
		return previous, true
	}

	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		return previous, false
	}
	return string(payload), true
}
