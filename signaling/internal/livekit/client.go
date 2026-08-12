// Package livekit habla con la API de administracion del SFU.
//
// Se usa solo para expulsar participantes cuando pierden el permiso: el resto
// del ciclo de vida de los rooms lo maneja LiveKit solo (se crean al entrar el
// host y se cierran al quedar vacios).
package livekit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/twitchtv/twirp"

	"github.com/soundvibe/media/signaling/internal/config"
)

// adminTokenTTL es la vida del token que firma cada llamada de administracion.
// Muy corta: se usa en el acto y no viaja a ningun cliente.
const adminTokenTTL = 30 * time.Second

type Client struct {
	apiURL string
	cfg    config.LiveKitConfig
	http   *http.Client
}

func New(cfg config.LiveKitConfig) *Client {
	return &Client{
		apiURL: cfg.APIURL,
		cfg:    cfg,
		http:   &http.Client{Timeout: 5 * time.Second},
	}
}

// Enabled indica si hay una URL de API configurada. Sin ella la expulsion no
// funciona, y el servicio arranca igual pero avisando.
func (c *Client) Enabled() bool { return c.apiURL != "" }

// ListParticipantIdentities devuelve las identidades presentes en el room.
//
// Un room inexistente no es un error: significa que no hay nadie a quien
// expulsar, que es un resultado perfectamente valido.
func (c *Client) ListParticipantIdentities(ctx context.Context, room string) ([]string, error) {
	client, err := c.roomClient(room)
	if err != nil {
		return nil, err
	}

	resp, err := client.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: room})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("no se pudo listar participantes de %q: %w", room, err)
	}

	identities := make([]string, 0, len(resp.Participants))
	for _, participant := range resp.Participants {
		if participant.Identity != "" {
			identities = append(identities, participant.Identity)
		}
	}
	return identities, nil
}

// RemoveParticipant desconecta a un participante del room.
//
// No se setea RevokeTokenTs: LiveKit por defecto invalida los tokens emitidos
// antes de ahora, que es justo lo que hace falta para que el expulsado no pueda
// volver a entrar con el mismo token que todavia no expiro.
func (c *Client) RemoveParticipant(ctx context.Context, room, identity string) error {
	client, err := c.roomClient(room)
	if err != nil {
		return err
	}

	if _, err := client.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{
		Room:     room,
		Identity: identity,
	}); err != nil {
		if isNotFound(err) {
			// Ya no estaba: el objetivo se cumple igual.
			return nil
		}
		return fmt.Errorf("no se pudo expulsar a %q de %q: %w", identity, room, err)
	}
	return nil
}

// roomClient arma un cliente twirp autenticado con un token de admin acotado a
// ese room. Se firma uno por llamada porque el grant RoomAdmin es por room.
func (c *Client) roomClient(room string) (livekit.RoomService, error) {
	token, err := c.adminToken(room)
	if err != nil {
		return nil, err
	}
	// Cliente JSON y no protobuf: al mismo volumen la diferencia es nula, y en
	// JSON los errores del SFU se leen directamente en los logs.
	return livekit.NewRoomServiceJSONClient(c.apiURL,
		&authedClient{inner: c.http, token: token}), nil
}

func (c *Client) adminToken(room string) (string, error) {
	grant := &auth.VideoGrant{RoomAdmin: true, Room: room}

	token, err := auth.NewAccessToken(c.cfg.APIKey, c.cfg.APISecret).
		SetVideoGrant(grant).
		SetIdentity("soundvibe-signaling").
		SetValidFor(adminTokenTTL).
		ToJWT()
	if err != nil {
		return "", fmt.Errorf("no se pudo firmar el token de admin de LiveKit: %w", err)
	}
	return token, nil
}

// authedClient inyecta el header Authorization en cada llamada twirp.
type authedClient struct {
	inner *http.Client
	token string
}

func (a *authedClient) Do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+a.token)
	return a.inner.Do(req)
}

func isNotFound(err error) bool {
	var twerr twirp.Error
	if errors.As(err, &twerr) {
		return twerr.Code() == twirp.NotFound
	}
	return false
}
