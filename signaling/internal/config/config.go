// Package config carga la configuracion del servicio de signaling desde
// variables de entorno.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env  string
	Port string

	Core    CoreConfig
	LiveKit LiveKitConfig
}

// CoreConfig apunta a soundvibe-core, que es la autoridad sobre identidad y
// permisos. Este servicio no tiene base de datos propia.
type CoreConfig struct {
	BaseURL string
	APIKey  string
	Timeout time.Duration
}

type LiveKitConfig struct {
	// URL es la que se le entrega al cliente para conectarse al SFU
	// (wss://... en produccion).
	URL       string
	APIKey    string
	APISecret string
	// TokenTTL es la vida del token de acceso a LiveKit. Corta a proposito: el
	// token solo hace falta para entrar al room.
	TokenTTL time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		Env:  env("APP_ENV", "development"),
		Port: env("PORT", "8092"),
		Core: CoreConfig{
			BaseURL: strings.TrimRight(env("CORE_BASE_URL", "http://sv-core-api:8091"), "/"),
			APIKey:  os.Getenv("INTERNAL_API_KEY"),
		},
		LiveKit: LiveKitConfig{
			URL:       os.Getenv("LIVEKIT_URL"),
			APIKey:    os.Getenv("LIVEKIT_API_KEY"),
			APISecret: os.Getenv("LIVEKIT_API_SECRET"),
		},
	}

	coreTimeout, err := envInt("CORE_TIMEOUT_SECONDS", 5)
	if err != nil {
		return Config{}, err
	}
	cfg.Core.Timeout = time.Duration(coreTimeout) * time.Second

	tokenTTL, err := envInt("LIVEKIT_TOKEN_TTL_MINUTES", 10)
	if err != nil {
		return Config{}, err
	}
	cfg.LiveKit.TokenTTL = time.Duration(tokenTTL) * time.Minute

	var missing []string
	if cfg.Core.BaseURL == "" {
		missing = append(missing, "CORE_BASE_URL")
	}
	// La misma clave que soundvibe-core: sin ella no se puede consultar ni la
	// identidad ni los permisos, y el servicio no tendria nada que hacer.
	if len(cfg.Core.APIKey) < 16 {
		missing = append(missing, "INTERNAL_API_KEY (minimo 16 caracteres, igual que en soundvibe-core)")
	}
	if cfg.LiveKit.URL == "" {
		missing = append(missing, "LIVEKIT_URL")
	}
	if cfg.LiveKit.APIKey == "" {
		missing = append(missing, "LIVEKIT_API_KEY")
	}
	if len(cfg.LiveKit.APISecret) < 32 {
		missing = append(missing, "LIVEKIT_API_SECRET (minimo 32 caracteres)")
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("configuracion invalida, falta o es debil: %s",
			strings.Join(missing, ", "))
	}

	return cfg, nil
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s debe ser un entero: %w", key, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s debe ser mayor que cero", key)
	}
	return n, nil
}
