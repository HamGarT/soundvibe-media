// Package relay publica en el room de LiveKit el audio que el host manda por
// WebSocket, sin recodificarlo en ningun punto.
//
// El telefono codifica Opus y manda los frames tal cual; aca se reenvian tal
// cual al SFU, que tampoco transcodifica. Nada entre el reproductor del host y
// el decodificador del oyente vuelve a codificar el audio, que es lo que
// mantiene el costo de CPU del VPS proporcional a "reenviar paquetes" en vez de
// a "recodificar por host".
//
// El motivo de que el audio pase por aca en vez de salir directo del telefono
// es que el SDK de WebRTC de Android solo publica audio a traves de su
// AudioDeviceModule, que **siempre abre el microfono** (su
// `JavaAudioDeviceModule` crea un `WebRtcAudioRecord` sin opcion de
// desactivarlo, y un ADM propio exige C++ porque
// `getNativeAudioDeviceModulePointer` devuelve un puntero nativo). Una app de
// musica que enciende el indicador de microfono mientras transmite se lee como
// que espia al usuario, asi que el telefono no habla WebRTC: habla WebSocket
// con este servicio, y este servicio es el que entra al room.
package relay

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/soundvibe/media/signaling/internal/config"
	"github.com/soundvibe/media/signaling/internal/rooms"
)

const (
	// opusSampleRate y opusChannels son los del stream que manda el telefono.
	// Tienen que coincidir con lo que codifica el cliente: si no, el oyente
	// escucha la musica al tono y a la velocidad equivocados.
	opusSampleRate = 48_000
	opusChannels   = 2

	// frameDuration es el tamano de frame por defecto de Opus, y el que asume la
	// paquetizacion de WebRTC.
	frameDuration = 20 * time.Millisecond

	trackName = "soundvibe-player"
)

// ErrSessionClosed se devuelve al escribir en una sesion ya cerrada.
var ErrSessionClosed = errors.New("la sesion de transmision esta cerrada")

// Relay lleva la cuenta de que hosts estan transmitiendo ahora mismo.
type Relay struct {
	cfg    config.LiveKitConfig
	minter *rooms.Minter

	mu       sync.Mutex
	sessions map[uuid.UUID]*Session
}

func New(cfg config.LiveKitConfig, minter *rooms.Minter) *Relay {
	return &Relay{
		cfg:      cfg,
		minter:   minter,
		sessions: make(map[uuid.UUID]*Session),
	}
}

// Session es la transmision viva de un host: una conexion al room y un track de
// audio publicandose.
type Session struct {
	hostID uuid.UUID

	room  *lksdk.Room
	track *lksdk.LocalTrack

	mu     sync.Mutex
	closed bool

	frames uint64
}

// Start abre la transmision de un host y devuelve la sesion lista para recibir
// frames.
//
// Si el host ya tenia una sesion, se cierra primero. Pasa de verdad — cambio de
// red, la app que se reabre — y dos publicadores con la misma identidad en el
// mismo room se pisan: el SFU echa al anterior y el oyente escucha un corte.
func (r *Relay) Start(hostID uuid.UUID, username string) (*Session, error) {
	if hostID == uuid.Nil {
		return nil, fmt.Errorf("hostID es obligatorio")
	}

	r.mu.Lock()
	if previous, ok := r.sessions[hostID]; ok {
		delete(r.sessions, hostID)
		r.mu.Unlock()
		previous.Close()
		r.mu.Lock()
	}
	r.mu.Unlock()

	// El token se firma con el mismo minter que usa /rooms/join, con rol host:
	// asi los grants (publica, no se suscribe, no administra) viven en un solo
	// lugar y no pueden divergir entre el camino del cliente y este.
	token, err := r.minter.Mint(hostID, hostID, username, rooms.RoleHost)
	if err != nil {
		return nil, fmt.Errorf("no se pudo firmar el token del host: %w", err)
	}

	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: opusSampleRate,
		Channels:  opusChannels,
	})
	if err != nil {
		return nil, fmt.Errorf("no se pudo crear el track de audio: %w", err)
	}

	room, err := lksdk.ConnectToRoomWithToken(r.cfg.URL, token.Token, nil)
	if err != nil {
		return nil, fmt.Errorf("no se pudo conectar al room %s: %w", token.Room, err)
	}

	_, err = room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name:   trackName,
		Source: livekit.TrackSource_MICROPHONE,
		// Musica, no voz. DTX corta la transmision en los silencios, que en una
		// cancion son parte de la cancion; y sin Stereo el SFU anuncia el track
		// como mono y se pierde la mitad de la mezcla. Del lado de Android estas
		// dos no existen como opciones de publicacion, que es otra razon por la
		// que publicar desde aca sale mejor.
		DisableDTX: true,
		Stereo:     true,
	})
	if err != nil {
		room.Disconnect()
		return nil, fmt.Errorf("no se pudo publicar el track en %s: %w", token.Room, err)
	}

	session := &Session{hostID: hostID, room: room, track: track}

	r.mu.Lock()
	r.sessions[hostID] = session
	r.mu.Unlock()

	return session, nil
}

// Stop cierra la transmision de un host, si tenia una.
func (r *Relay) Stop(hostID uuid.UUID) {
	r.mu.Lock()
	session, ok := r.sessions[hostID]
	if ok {
		delete(r.sessions, hostID)
	}
	r.mu.Unlock()

	if ok {
		session.Close()
	}
}

// IsBroadcasting dice si un host esta transmitiendo ahora mismo.
//
// Es la respuesta a "quien esta activo" para la pantalla de amigos, y viene de
// aca y no de `ListRooms` de LiveKit a proposito: con publicacion bajo demanda
// no hay ningun room abierto mientras la gente escucha sola, asi que preguntarle
// al SFU daria a todo el mundo por inactivo.
func (r *Relay) IsBroadcasting(hostID uuid.UUID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.sessions[hostID]
	return ok
}

// Broadcasting devuelve los hosts que estan transmitiendo ahora mismo.
func (r *Relay) Broadcasting() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()

	hosts := make([]uuid.UUID, 0, len(r.sessions))
	for hostID := range r.sessions {
		hosts = append(hosts, hostID)
	}
	return hosts
}

// CloseAll cierra todas las sesiones. Se usa al apagar el servicio, para que los
// oyentes reciban una desconexion limpia en vez de quedarse en silencio.
func (r *Relay) CloseAll() {
	r.mu.Lock()
	sessions := make([]*Session, 0, len(r.sessions))
	for hostID, session := range r.sessions {
		sessions = append(sessions, session)
		delete(r.sessions, hostID)
	}
	r.mu.Unlock()

	for _, session := range sessions {
		session.Close()
	}
}

// HostID identifica al host de la sesion.
func (s *Session) HostID() uuid.UUID { return s.hostID }

// Frames son los frames de audio reenviados hasta ahora.
func (s *Session) Frames() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frames
}

// WriteFrame reenvia un frame de Opus ya codificado al room.
//
// El frame se pasa tal cual llego del telefono: aca no se decodifica, no se
// mezcla y no se vuelve a codificar.
func (s *Session) WriteFrame(frame []byte) error {
	if len(frame) == 0 {
		return nil
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrSessionClosed
	}
	s.frames++
	s.mu.Unlock()

	return s.track.WriteSample(media.Sample{
		Data:     frame,
		Duration: frameDuration,
	}, nil)
}

// Close deja de publicar y sale del room. Es idempotente.
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	_ = s.track.Close()
	s.room.Disconnect()
}
