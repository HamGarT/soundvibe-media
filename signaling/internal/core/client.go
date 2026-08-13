// Package core es el cliente HTTP de soundvibe-core, que es la autoridad sobre
// identidad de usuarios y permisos de escucha.
//
// Regla de oro de este paquete: **fallar cerrado**. Cualquier resultado que no
// sea un 200 explicito — 403, 500, timeout, DNS caido, JSON corrupto — se trata
// como "no autorizado". Un error de red no puede terminar en un permiso
// concedido.
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/soundvibe/media/signaling/internal/config"
)

// maxResponseBytes acota lo que se lee de core: es un servicio de confianza,
// pero un cliente HTTP sin limite es una fuga de memoria esperando ocurrir.
const maxResponseBytes = 1 << 20

// ErrUnauthenticated significa que el access token no es valido o expiro.
var ErrUnauthenticated = errors.New("el access token no es valido")

// ErrUnavailable significa que no se pudo obtener una respuesta util de core.
// Quien lo reciba debe denegar, no reintentar indefinidamente.
var ErrUnavailable = errors.New("soundvibe-core no esta disponible")

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func New(cfg config.CoreConfig) *Client {
	return &Client{
		baseURL: cfg.BaseURL,
		apiKey:  cfg.APIKey,
		http: &http.Client{
			Timeout: cfg.Timeout,
			// Sin keep-alives ociosos de sobra: el trafico a core es de un
			// request corto por join.
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// Identity es la respuesta de /internal/introspect. No incluye email: core
// deliberadamente no lo comparte con este servicio.
type Identity struct {
	UserID       uuid.UUID `json:"user_id"`
	Username     string    `json:"username"`
	ShareDefault string    `json:"share_default"`
}

// Introspect resuelve un access token de usuario a su identidad.
//
// Se delega en core en vez de verificar el JWT localmente porque la firma es
// HS256 (simetrica): tener el secreto aca permitiria emitir tokens en nombre de
// cualquier usuario, y este servicio no necesita ese poder.
func (c *Client) Introspect(ctx context.Context, accessToken string) (Identity, error) {
	body, err := json.Marshal(map[string]string{"access_token": accessToken})
	if err != nil {
		return Identity{}, fmt.Errorf("%w: no se pudo armar el request: %v", ErrUnavailable, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/internal/introspect", bytes.NewReader(body))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Api-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer drainAndClose(resp)

	switch resp.StatusCode {
	case http.StatusOK:
		var identity Identity
		if err := decode(resp, &identity); err != nil {
			return Identity{}, err
		}
		if identity.UserID == uuid.Nil {
			return Identity{}, fmt.Errorf("%w: core devolvio una identidad sin user_id", ErrUnavailable)
		}
		return identity, nil

	case http.StatusUnauthorized:
		// Ojo: un 401 puede significar "el token del usuario es invalido" o
		// "nuestra INTERNAL_API_KEY es incorrecta". Se distinguen por el codigo
		// de error del body, porque confundirlos manda al usuario a
		// reautenticarse cuando el problema es de configuracion del servidor.
		if code := errorCode(resp); code == "invalid_access_token" {
			return Identity{}, ErrUnauthenticated
		}
		return Identity{}, fmt.Errorf(
			"%w: core rechazo nuestra INTERNAL_API_KEY (revisar que coincida en los dos servicios)",
			ErrUnavailable)

	case http.StatusUnprocessableEntity:
		// Token vacio o malformado: el validador de core lo rechazo antes de
		// intentar verificarlo.
		return Identity{}, ErrUnauthenticated

	default:
		return Identity{}, fmt.Errorf("%w: core respondio %d", ErrUnavailable, resp.StatusCode)
	}
}

// CanListen pregunta a core si `listener` puede escuchar la actividad de `host`.
//
// Devuelve (false, nil) cuando core dice que no, y (false, error) cuando no se
// pudo preguntar. En los dos casos el llamador deniega; se distinguen solo para
// poder loguear la diferencia y devolver el status correcto al cliente.
func (c *Client) CanListen(ctx context.Context, hostID, listenerID uuid.UUID) (allowed bool, reason string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/internal/listening-permission", nil)
	if err != nil {
		return false, "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	query := req.URL.Query()
	query.Set("host", hostID.String())
	query.Set("listener", listenerID.String())
	req.URL.RawQuery = query.Encode()
	req.Header.Set("X-Internal-Api-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer drainAndClose(resp)

	var decision struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason"`
	}

	switch resp.StatusCode {
	case http.StatusOK:
		if err := decode(resp, &decision); err != nil {
			return false, "", err
		}
		if !decision.Allowed {
			// Incoherencia entre status y body: se falla cerrado y se cree al
			// body, que es el que lleva la decision explicita.
			return false, decision.Reason, fmt.Errorf(
				"%w: core respondio 200 con allowed=false", ErrUnavailable)
		}
		return true, decision.Reason, nil

	case http.StatusForbidden:
		// Denegacion legitima y esperada. El motivo viene en el body.
		_ = decode(resp, &decision)
		return false, decision.Reason, nil

	case http.StatusUnauthorized:
		return false, "", fmt.Errorf(
			"%w: core rechazo nuestra INTERNAL_API_KEY (revisar que coincida en los dos servicios)",
			ErrUnavailable)

	default:
		return false, "", fmt.Errorf("%w: core respondio %d", ErrUnavailable, resp.StatusCode)
	}
}

// Friend es un amigo aceptado del usuario, tal como lo devuelve core.
type Friend struct {
	UserID      uuid.UUID `json:"user_id"`
	Username    string    `json:"username"`
	DisplayName *string   `json:"display_name"`
	AvatarURL   *string   `json:"avatar_url"`
}

// Friends lista los amigos aceptados del usuario duenio de `accessToken`.
//
// Va con el token del propio usuario y no con la API key interna a proposito:
// core ya expone /friendships autenticado, y usarlo evita agregar un endpoint
// interno que devuelva la lista de amigos de cualquiera.
func (c *Client) Friends(ctx context.Context, accessToken string) ([]Friend, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/friendships", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer drainAndClose(resp)

	switch resp.StatusCode {
	case http.StatusOK:
		var body struct {
			Friends []Friend `json:"friends"`
		}
		if err := decode(resp, &body); err != nil {
			return nil, err
		}
		return body.Friends, nil

	case http.StatusUnauthorized:
		return nil, ErrUnauthenticated

	default:
		return nil, fmt.Errorf("%w: core respondio %d en /friendships", ErrUnavailable, resp.StatusCode)
	}
}

// CanListenBatch resuelve de una vez a cuales de `hosts` puede escuchar
// `listener`, con una sola llamada en vez de una por host.
//
// Devuelve solo los permitidos: quien llama arma la pantalla de amigos activos
// con eso, y los denegados no tienen por que aparecer ni siquiera como ausentes.
func (c *Client) CanListenBatch(ctx context.Context, listenerID uuid.UUID, hosts []uuid.UUID) ([]uuid.UUID, error) {
	if len(hosts) == 0 {
		return nil, nil
	}

	payload := struct {
		Listener uuid.UUID   `json:"listener"`
		Hosts    []uuid.UUID `json:"hosts"`
	}{Listener: listenerID, Hosts: hosts}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/internal/listening-permissions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Api-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer drainAndClose(resp)

	switch resp.StatusCode {
	case http.StatusOK:
		var decoded struct {
			Results []struct {
				HostID  uuid.UUID `json:"host_id"`
				Allowed bool      `json:"allowed"`
			} `json:"results"`
		}
		if err := decode(resp, &decoded); err != nil {
			return nil, err
		}
		allowed := make([]uuid.UUID, 0, len(decoded.Results))
		for _, result := range decoded.Results {
			if result.Allowed {
				allowed = append(allowed, result.HostID)
			}
		}
		return allowed, nil

	case http.StatusUnauthorized:
		return nil, fmt.Errorf(
			"%w: core rechazo nuestra INTERNAL_API_KEY (revisar que coincida en los dos servicios)",
			ErrUnavailable)

	default:
		return nil, fmt.Errorf("%w: core respondio %d", ErrUnavailable, resp.StatusCode)
	}
}

// Health consulta el /health de core. Solo se usa para diagnostico.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: core respondio %d en /health", ErrUnavailable, resp.StatusCode)
	}
	return nil
}

func decode(resp *http.Response, dst any) error {
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(dst); err != nil {
		return fmt.Errorf("%w: respuesta ilegible de core: %v", ErrUnavailable, err)
	}
	return nil
}

// errorCode lee el campo error.code del formato de error de core.
func errorCode(resp *http.Response) string {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := decode(resp, &body); err != nil {
		return ""
	}
	return body.Error.Code
}

// drainAndClose vacia y cierra el body para que la conexion vuelva al pool en
// vez de quedar colgada.
func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	_ = resp.Body.Close()
}
