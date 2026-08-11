// Package rooms traduce "este usuario quiere escuchar a este host" en un token
// de LiveKit con los permisos justos.
package rooms

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/livekit/protocol/auth"

	"github.com/soundvibe/media/signaling/internal/config"
)

// Role es el papel del participante dentro del room.
type Role string

const (
	// RoleHost es el dueno de la actividad: es el unico que publica audio.
	RoleHost Role = "host"
	// RoleListener solo escucha.
	RoleListener Role = "listener"
)

// roomPrefix arma el nombre del room a partir del id del host. Hay un room por
// host, no por sesion: los oyentes se suman al room del host que quieren
// escuchar.
const roomPrefix = "listen:"

// Name devuelve el nombre del room de un host. Es una convencion compartida con
// el cliente; cambiarla rompe a las apps ya publicadas.
func Name(hostID uuid.UUID) string {
	return roomPrefix + hostID.String()
}

// Token es lo que el cliente necesita para conectarse al SFU.
type Token struct {
	LiveKitURL string    `json:"livekit_url"`
	Token      string    `json:"token"`
	Room       string    `json:"room"`
	Identity   string    `json:"identity"`
	Role       Role      `json:"role"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Minter firma tokens de LiveKit.
type Minter struct {
	cfg config.LiveKitConfig
	now func() time.Time
}

func NewMinter(cfg config.LiveKitConfig) *Minter {
	return &Minter{cfg: cfg, now: time.Now}
}

// Mint firma un token para que `user` entre al room de `host` con el rol dado.
//
// Los grants son la unica barrera real dentro del room: la consulta de permisos
// a core decide si se entrega un token, pero una vez que el cliente esta dentro
// solo los grants impiden que un oyente publique audio en el room de otro.
func (m *Minter) Mint(hostID, userID uuid.UUID, username string, role Role) (Token, error) {
	if hostID == uuid.Nil || userID == uuid.Nil {
		return Token{}, fmt.Errorf("hostID y userID son obligatorios")
	}

	room := Name(hostID)
	canPublish := role == RoleHost

	grant := &auth.VideoGrant{
		RoomJoin: true,
		Room:     room,
		// El host publica y no se suscribe a nada; el oyente al reves. Ningun
		// rol puede publicar datos: este room es solo audio.
		CanPublish:     boolPtr(canPublish),
		CanSubscribe:   boolPtr(!canPublish),
		CanPublishData: boolPtr(false),
		// Sin permiso para administrar el room ni para listar otros rooms: un
		// cliente no tiene por que poder expulsar participantes ni descubrir
		// quien mas esta transmitiendo.
		RoomAdmin:  false,
		RoomList:   false,
		RoomCreate: false,
	}

	ttl := m.cfg.TokenTTL
	expiresAt := m.now().UTC().Add(ttl)

	at := auth.NewAccessToken(m.cfg.APIKey, m.cfg.APISecret).
		SetVideoGrant(grant).
		// La identidad es el UUID del usuario en core, no el username: el
		// username puede cambiar y el UUID es lo que entiende el endpoint de
		// permisos.
		SetIdentity(userID.String()).
		SetName(username).
		SetValidFor(ttl)

	signed, err := at.ToJWT()
	if err != nil {
		return Token{}, fmt.Errorf("no se pudo firmar el token de LiveKit: %w", err)
	}

	return Token{
		LiveKitURL: m.cfg.URL,
		Token:      signed,
		Room:       room,
		Identity:   userID.String(),
		Role:       role,
		ExpiresAt:  expiresAt,
	}, nil
}

func boolPtr(v bool) *bool { return &v }
