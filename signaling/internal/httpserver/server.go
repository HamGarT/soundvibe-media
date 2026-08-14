// Package httpserver arma el router del servicio de signaling.
//
// El servicio es deliberadamente delgado y no tiene base de datos: soundvibe-core
// es la autoridad sobre identidad y permisos, y LiveKit sobre el audio; esto es
// solo el portero entre los dos.
//
// El unico estado que si guarda vive en memoria y es cierto solo mientras dura
// una conexion: que hosts estan transmitiendo (relay) y que esta sonando en cada
// telefono (presence). Los dos se pierden al reiniciar, que es lo correcto —
// persistirlos reviviria una multitud de gente "activa" que ya no lo esta.
package httpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/soundvibe/media/signaling/internal/config"
	"github.com/soundvibe/media/signaling/internal/core"
	"github.com/soundvibe/media/signaling/internal/livekit"
	"github.com/soundvibe/media/signaling/internal/presence"
	"github.com/soundvibe/media/signaling/internal/relay"
	"github.com/soundvibe/media/signaling/internal/rooms"
)

type Server struct {
	core          *core.Client
	livekit       *livekit.Client
	minter        *rooms.Minter
	relay         *relay.Relay
	presenceStore *presence.Store
	audience      *audience
}

// New arma el handler HTTP completo del servicio.
func New(cfg config.Config, coreClient *core.Client, livekitClient *livekit.Client) http.Handler {
	minter := rooms.NewMinter(cfg.LiveKit)
	presenceStore := presence.NewStore()

	// Vencer no tiene evento propio: a nadie le avisan cuando a un telefono se le
	// acaba la bateria, y el host que pausa simplemente deja de latir. El barrido
	// convierte eso en un cambio, que es lo que despierta a los suscriptores.
	//
	// context.Background y sin cancelacion: vive lo que vive el proceso, igual que
	// el router. Nada aca sobrevive a un reinicio ni tiene por que hacerlo.
	presenceStore.StartSweeper(context.Background(), presence.SweepInterval)

	s := &Server{
		core:          coreClient,
		livekit:       livekitClient,
		minter:        minter,
		relay:         relay.New(cfg.LiveKit, minter),
		presenceStore: presenceStore,
		audience:      newAudience(livekitClient, presenceStore),
	}

	// Que un oyente se vaya no genera ningun evento en este servicio, asi que la
	// unica forma de saber que un host dejo de tener audiencia es preguntarle al
	// SFU cada tanto.
	s.audience.StartReconciler(context.Background(), audienceReconcile)

	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)

	// El timeout se aplica por grupo y no a todo el router: una transmision es
	// una conexion que dura lo que dure la sesion de escucha, y un
	// middleware.Timeout global la cortaria a los 20 segundos.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(20 * time.Second))

		r.Get("/health", s.health)
		r.Post("/rooms/join", s.join)
		// Que amigos estan transmitiendo ahora, ya filtrados por permiso. Es lo
		// que pinta la pantalla de amigos.
		r.Get("/rooms/active", s.active)

		// Ruta servicio-a-servicio: la llama soundvibe-core cuando cambian los
		// permisos. No la usa ningun cliente final.
		r.Route("/internal", func(r chi.Router) {
			r.Use(internalAPIKeyMiddleware(cfg.Core.APIKey))
			r.Post("/revoke", s.revoke)
		})
	})

	// Los dos WebSocket van fuera del grupo con timeout, por lo de arriba: duran
	// lo que dure la sesion. La autenticacion va por header Authorization, que el
	// cliente nativo (OkHttp) si puede mandar en el handshake — un navegador no
	// podria.

	// El host manda su audio ya codificado por aca, recien cuando alguien lo
	// escucha.
	r.Get("/rooms/broadcast", s.broadcast)
	// Y por aca dice que esta sonando, todo el tiempo que reproduzca. Separado
	// del anterior a proposito: ver el comentario de s.presence.
	r.Get("/rooms/presence", s.presence)
	// El otro lado del mismo asunto: por aca el oyente recibe los cambios en vez
	// de preguntar cada tantos segundos.
	r.Get("/rooms/subscribe", s.subscribe)

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		fail(w, r, http.StatusNotFound, "not_found", "recurso no encontrado")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		fail(w, r, http.StatusMethodNotAllowed, "method_not_allowed",
			"metodo no permitido en esta ruta")
	})

	return r
}

// writeJSON escribe una respuesta JSON.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	buf, err := json.Marshal(payload)
	if err != nil {
		slog.ErrorContext(r.Context(), "no se pudo serializar la respuesta", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	if _, err := w.Write(buf); err != nil {
		slog.WarnContext(r.Context(), "no se pudo escribir la respuesta", "error", err)
	}
}

// fail responde con el mismo formato de error que usa soundvibe-core, para que
// el cliente tenga una sola forma de parsear errores.
func fail(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	body.Error.Code = code
	body.Error.Message = message
	writeJSON(w, r, status, body)
}

// requestLogger registra una linea por request. No registra el header
// Authorization: por ahi pasan los access tokens de los usuarios.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		defer func() {
			slog.InfoContext(r.Context(), "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", middleware.GetReqID(r.Context()),
				"remote_ip", r.RemoteAddr,
			)
		}()

		next.ServeHTTP(ww, r)
	})
}
