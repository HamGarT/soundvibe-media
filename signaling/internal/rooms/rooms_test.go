package rooms

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/soundvibe/media/signaling/internal/config"
)

const (
	testAPIKey    = "APItestkey"
	testAPISecret = "un-secreto-de-livekit-para-tests-0123456789"
)

func testMinter() *Minter {
	return NewMinter(config.LiveKitConfig{
		URL:       "wss://livekit.soundvibe.test",
		APIKey:    testAPIKey,
		APISecret: testAPISecret,
		TokenTTL:  10 * time.Minute,
	})
}

// videoGrant es la forma del claim `video` en el token de LiveKit. Se decodifica
// a mano en vez de usar los tipos del SDK para verificar exactamente lo que va
// a ver el servidor de LiveKit en el cable.
type videoGrant struct {
	RoomJoin       bool   `json:"roomJoin"`
	Room           string `json:"room"`
	CanPublish     *bool  `json:"canPublish"`
	CanSubscribe   *bool  `json:"canSubscribe"`
	CanPublishData *bool  `json:"canPublishData"`
	RoomAdmin      bool   `json:"roomAdmin"`
	RoomCreate     bool   `json:"roomCreate"`
	RoomList       bool   `json:"roomList"`
}

type livekitClaims struct {
	Subject string     `json:"sub"`
	Name    string     `json:"name"`
	Issuer  string     `json:"iss"`
	Video   videoGrant `json:"video"`
}

// parseClaims decodifica el payload del JWT sin verificar la firma.
func parseClaims(t *testing.T, token string) livekitClaims {
	t.Helper()

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("el token no tiene 3 partes: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("no se pudo decodificar el payload: %v", err)
	}

	var claims livekitClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("no se pudo parsear los claims (%s): %v", raw, err)
	}
	return claims
}

func TestName(t *testing.T) {
	id := uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	// El nombre del room es una convencion compartida con el cliente: si cambia,
	// las apps ya publicadas dejan de encontrar el room.
	if got, want := Name(id), "listen:6ba7b810-9dad-11d1-80b4-00c04fd430c8"; got != want {
		t.Errorf("Name() = %q, se esperaba %q", got, want)
	}
}

func TestMintHostCanPublishButNotSubscribe(t *testing.T) {
	hostID := uuid.New()
	token, err := testMinter().Mint(hostID, hostID, "ana", RoleHost)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if token.Role != RoleHost {
		t.Errorf("Role = %q, se esperaba host", token.Role)
	}
	if token.Room != Name(hostID) {
		t.Errorf("Room = %q, se esperaba %q", token.Room, Name(hostID))
	}
	if token.LiveKitURL != "wss://livekit.soundvibe.test" {
		t.Errorf("LiveKitURL = %q", token.LiveKitURL)
	}

	claims := parseClaims(t, token.Token)
	if claims.Video.Room != Name(hostID) {
		t.Errorf("grant.room = %q, se esperaba %q", claims.Video.Room, Name(hostID))
	}
	if !claims.Video.RoomJoin {
		t.Error("grant.roomJoin deberia ser true")
	}
	if claims.Video.CanPublish == nil || !*claims.Video.CanPublish {
		t.Error("el host deberia poder publicar")
	}
	if claims.Video.CanSubscribe == nil || *claims.Video.CanSubscribe {
		t.Error("el host no deberia suscribirse a nada")
	}
}

func TestMintListenerCanSubscribeButNotPublish(t *testing.T) {
	hostID := uuid.New()
	listenerID := uuid.New()

	token, err := testMinter().Mint(hostID, listenerID, "beto", RoleListener)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	claims := parseClaims(t, token.Token)

	// Esto es lo que impide que un oyente meta audio en el room de otro. En
	// LiveKit un canPublish ausente equivale a true, asi que tiene que estar
	// presente y en false.
	if claims.Video.CanPublish == nil {
		t.Fatal("canPublish ausente: LiveKit lo interpretaria como true")
	}
	if *claims.Video.CanPublish {
		t.Error("el oyente NO deberia poder publicar")
	}
	if claims.Video.CanSubscribe == nil || !*claims.Video.CanSubscribe {
		t.Error("el oyente deberia poder suscribirse")
	}
	if claims.Video.CanPublishData == nil || *claims.Video.CanPublishData {
		t.Error("el room es solo audio: canPublishData deberia ser false")
	}
	// El room es el del host, no el del oyente.
	if claims.Video.Room != Name(hostID) {
		t.Errorf("grant.room = %q, se esperaba el room del host %q", claims.Video.Room, Name(hostID))
	}
}

func TestMintGrantsNoAdminPowers(t *testing.T) {
	hostID := uuid.New()
	token, err := testMinter().Mint(hostID, uuid.New(), "beto", RoleListener)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	claims := parseClaims(t, token.Token)
	if claims.Video.RoomAdmin {
		t.Error("roomAdmin deberia ser false: un cliente no puede expulsar participantes")
	}
	if claims.Video.RoomList {
		t.Error("roomList deberia ser false: un cliente no puede descubrir quien transmite")
	}
	if claims.Video.RoomCreate {
		t.Error("roomCreate deberia ser false")
	}
}

func TestMintIdentityIsUserUUIDNotUsername(t *testing.T) {
	userID := uuid.New()
	token, err := testMinter().Mint(uuid.New(), userID, "ana", RoleListener)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if token.Identity != userID.String() {
		t.Errorf("Identity = %q, se esperaba el UUID %q", token.Identity, userID)
	}

	claims := parseClaims(t, token.Token)
	// El subject es el UUID: el username puede cambiar, el UUID es lo que
	// entiende el endpoint de permisos de core.
	if claims.Subject != userID.String() {
		t.Errorf("sub = %q, se esperaba %q", claims.Subject, userID)
	}
	if claims.Name != "ana" {
		t.Errorf("name = %q, se esperaba ana", claims.Name)
	}
}

func TestMintTokenIsSignedWithAPISecret(t *testing.T) {
	token, err := testMinter().Mint(uuid.New(), uuid.New(), "ana", RoleListener)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// El servidor de LiveKit valida la firma con el secreto compartido: si esto
	// falla, ningun cliente puede conectarse.
	parsed, err := jwt.Parse(token.Token, func(*jwt.Token) (any, error) {
		return []byte(testAPISecret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		t.Fatalf("el token no valida contra el API secret: %v", err)
	}
	if !parsed.Valid {
		t.Error("el token deberia ser valido")
	}

	claims := parseClaims(t, token.Token)
	if claims.Issuer != testAPIKey {
		t.Errorf("iss = %q, se esperaba el API key %q", claims.Issuer, testAPIKey)
	}
}

func TestMintRejectsWrongSecret(t *testing.T) {
	token, err := testMinter().Mint(uuid.New(), uuid.New(), "ana", RoleListener)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	_, err = jwt.Parse(token.Token, func(*jwt.Token) (any, error) {
		return []byte("otro-secreto-de-livekit-completamente-distinto"), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err == nil {
		t.Error("el token no deberia validar con otro secreto")
	}
}

func TestMintSetsShortExpiry(t *testing.T) {
	minter := testMinter()
	fixed := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	minter.now = func() time.Time { return fixed }

	token, err := minter.Mint(uuid.New(), uuid.New(), "ana", RoleListener)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if want := fixed.Add(10 * time.Minute); !token.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, se esperaba %v", token.ExpiresAt, want)
	}
}

func TestMintRejectsNilIDs(t *testing.T) {
	minter := testMinter()

	if _, err := minter.Mint(uuid.Nil, uuid.New(), "ana", RoleListener); err == nil {
		t.Error("se esperaba error con hostID nil")
	}
	if _, err := minter.Mint(uuid.New(), uuid.Nil, "ana", RoleListener); err == nil {
		t.Error("se esperaba error con userID nil")
	}
}
