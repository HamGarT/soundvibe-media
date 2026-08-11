// Package httpserver arma el router del servicio de signaling.
//
// El servicio es deliberadamente delgado y sin estado: no tiene base de datos.
// soundvibe-core es la autoridad sobre identidad y permisos, y LiveKit sobre el
// audio; esto es solo el portero entre los dos.
package httpserver

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/soundvibe/media/signaling/internal/config"
	"github.com/soundvibe/media/signaling/internal/core"
	"github.com/soundvibe/media/signaling/internal/rooms"
)

type Server struct {
	core   *core.Client
	minter *rooms.Minter
}

// New arma el handler HTTP completo del servicio.
func New(cfg config.Config, coreClient *core.Client) http.Handler {
	s := &Server{
		core:   coreClient,
		minter: rooms.NewMinter(cfg.LiveKit),
	}

	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(requestLogger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(20 * time.Second))

	r.Get("/health", s.health)
	r.Post("/rooms/join", s.join)

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
