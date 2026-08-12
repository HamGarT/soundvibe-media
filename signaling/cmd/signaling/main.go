// Command signaling es el portero entre la app y LiveKit.
//
// No maneja audio (eso es LiveKit) ni usuarios (eso es soundvibe-core): recibe
// "quiero entrar al room de X", le pregunta a core quien es el usuario y si
// tiene permiso, y solo entonces firma un token de LiveKit.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/soundvibe/media/signaling/internal/config"
	"github.com/soundvibe/media/signaling/internal/core"
	"github.com/soundvibe/media/signaling/internal/httpserver"
	"github.com/soundvibe/media/signaling/internal/livekit"
)

func main() {
	if err := run(); err != nil {
		slog.Error("el servicio de signaling no pudo arrancar", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	setupLogger(cfg.Env)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	coreClient := core.New(cfg.Core)
	livekitClient := livekit.New(cfg.LiveKit)

	if !livekitClient.Enabled() {
		slog.Warn("LIVEKIT_API_URL sin configurar: la revocacion en vivo queda deshabilitada, " +
			"un oyente que pierde el permiso sigue conectado hasta que se va o expira su sesion")
	}

	// No se aborta el arranque si core no responde todavia: los dos stacks se
	// levantan por separado y el orden no esta garantizado. Se avisa y se sigue;
	// /health reporta el estado real.
	checkCtx, cancelCheck := context.WithTimeout(ctx, 5*time.Second)
	if err := coreClient.Health(checkCtx); err != nil {
		slog.Warn("soundvibe-core no responde al arrancar; los joins van a fallar cerrado hasta que vuelva",
			"core_url", cfg.Core.BaseURL, "error", err)
	} else {
		slog.Info("soundvibe-core alcanzable", "core_url", cfg.Core.BaseURL)
	}
	cancelCheck()

	srv := &http.Server{
		Addr:              net.JoinHostPort("", cfg.Port),
		Handler:           httpserver.New(cfg, coreClient, livekitClient),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("signaling escuchando", "port", cfg.Port, "env", cfg.Env,
			"livekit_url", cfg.LiveKit.URL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("señal de apagado recibida, cerrando")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	slog.Info("signaling cerrado")
	return nil
}

func setupLogger(env string) {
	level := slog.LevelInfo
	if env == "development" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
}
