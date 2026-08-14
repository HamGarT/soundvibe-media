// Package presence lleva la cuenta de quien esta escuchando algo ahora mismo y
// que esta sonando.
//
// Es la fuente de verdad de "quien esta activo", y no `ListRooms` de LiveKit: el
// host publica audio **bajo demanda**, asi que mientras nadie lo escucha no hay
// ninguna room abierta y el SFU daria a todo el mundo por inactivo. La presencia
// viaja por su propio WebSocket, liviano, que el telefono mantiene abierto
// mientras reproduce — el de audio recien se abre cuando alguien toca tune in.
//
// Este paquete NO decide quien puede ver a quien. Guarda lo que anuncia cada
// host y nada mas; el filtro de permisos vive en /rooms/active, que pregunta a
// core antes de devolver un solo titulo. Mantenerlos separados es a proposito:
// un store que ademas filtrara tendria dos motivos para cambiar y seria el lugar
// natural donde olvidarse de preguntar.
package presence

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// SweepInterval es cada cuanto se barren las entradas vencidas.
//
// Bastante mas fino que el TTL: es lo que separa "el host se fue" de "el
// suscriptor se entera", y con el barrido cada 5 s el retraso que agrega es
// despreciable al lado de los 45 s que tarda en vencer.
const SweepInterval = 5 * time.Second

// commandBuffer son las ordenes que se le guardan a un host antes de empezar a
// descartarlas.
//
// Chico a proposito: las ordenes son de estado ("transmiti" / "no transmitas"),
// no de historia. Si se acumulan es porque el telefono no esta leyendo, y en ese
// caso lo ultimo que sirve es entregarle una cola de ordenes viejas cuando
// vuelva — la reconciliacion le va a decir el estado correcto igual.
const commandBuffer = 4

// TTL es cuanto vale un anuncio sin que lo refresquen.
//
// Existe porque un telefono que pierde la red no cierra el WebSocket: se queda
// mudo. Sin vencimiento su duenio seguiria apareciendo activo para siempre, y la
// pantalla de amigos ofreceria entrar a escuchar a alguien que no esta. El
// cliente refresca cada [Heartbeat]; el margen cubre un par de latidos perdidos
// antes de dar a alguien por ido.
const TTL = 45 * time.Second

// Heartbeat es cada cuanto deberia refrescar el cliente. Vive aca, al lado del
// TTL, porque los dos numeros solo tienen sentido juntos.
const Heartbeat = 15 * time.Second

// Track es lo que suena en el telefono del host.
//
// Todos los campos son opcionales salvo el titulo: la biblioteca sale de los
// tags de archivos locales, que muchas veces vienen a medias, y media cancion
// sin artista sigue siendo mejor que no mostrar nada.
type Track struct {
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Album      string `json:"album"`
	DurationMs int64  `json:"duration_ms"`
	// PositionMs es donde iba la reproduccion en [Entry.UpdatedAt], no ahora.
	// Quien lo muestre tiene que extrapolar con el tiempo transcurrido, porque
	// entre latidos pasan segundos.
	PositionMs int64 `json:"position_ms"`
}

// Entry es el anuncio vivo de un host.
type Entry struct {
	HostID    uuid.UUID
	Username  string
	Track     Track
	UpdatedAt time.Time
}

// Command es una orden que el servidor le manda al telefono del host por el
// mismo socket por el que este anuncia.
//
// El socket de presencia se abre apenas el usuario decide compartir y se queda
// abierto, asi que es el unico canal que ya esta ahi cuando hace falta avisarle
// algo — y avisarle es justamente lo que permite no transmitir hasta que haya
// alguien escuchando.
type Command string

const (
	// CommandStartBroadcast le dice al host que alguien quiere escucharlo y que
	// empiece a publicar audio.
	CommandStartBroadcast Command = "start_broadcast"

	// CommandStopBroadcast le dice que se fue el ultimo oyente y puede dejar de
	// gastar bateria y datos.
	CommandStopBroadcast Command = "stop_broadcast"

	// CommandListeners lleva quien lo esta escuchando ahora mismo.
	//
	// Va por el mismo canal que las ordenes porque es la misma conversacion: el
	// servidor es el unico que sabe quien esta en el room, y este socket ya esta
	// abierto. Al host se le dice quien lo escucha y no solo cuantos — es su
	// propia audiencia, y la pantalla muestra sus caras.
	CommandListeners Command = "listeners"
)

// Message es lo que se le manda al host.
//
// Lleva el tipo y, cuando corresponde, los datos. Un struct en vez de un string
// suelto porque [CommandListeners] necesita acompaniar una lista, y meterla en
// el nombre habria sido inventar un formato dentro de otro.
type Message struct {
	Type Command `json:"type"`

	// Listeners son los ids de quienes escuchan, solo en [CommandListeners].
	Listeners []string `json:"listeners,omitempty"`
}

// Store guarda el ultimo anuncio de cada host.
//
// Todo en memoria y a proposito: la presencia es cierta solo mientras el socket
// esta abierto, y los sockets viven en este proceso. Persistirla sobreviviria a
// un reinicio como un monton de gente "activa" que no lo esta.
type Store struct {
	mu      sync.RWMutex
	entries map[uuid.UUID]Entry

	// watchers son los suscriptores que quieren enterarse de los cambios. El
	// canal lleva struct{} y no el estado: cada suscriptor ve cosas distintas
	// segun sus amistades y permisos, asi que lo unico que se puede compartir
	// entre todos es "algo cambio, volve a mirar".
	watchers map[chan struct{}]struct{}

	// commands es el canal de vuelta hacia cada host con el socket abierto, para
	// decirle que empiece o deje de transmitir.
	commands map[uuid.UUID]chan Message

	// now es inyectable para que los tests puedan hacer vencer entradas sin
	// dormir 45 segundos.
	now func() time.Time
}

func NewStore() *Store {
	return &Store{
		entries:  make(map[uuid.UUID]Entry),
		watchers: make(map[chan struct{}]struct{}),
		commands: make(map[uuid.UUID]chan Message),
		now:      time.Now,
	}
}

// Attach registra el canal por el que se le mandan ordenes a un host, y devuelve
// la funcion para darlo de baja.
//
// Lo llama el handler del socket de presencia al abrirse. Un host que reconecta
// reemplaza su canal anterior: el viejo pertenece a una conexion que ya no
// existe, y dejarlo puesto mandaria ordenes a un socket muerto.
func (s *Store) Attach(hostID uuid.UUID) (<-chan Message, func()) {
	ch := make(chan Message, commandBuffer)

	s.mu.Lock()
	if previous, ok := s.commands[hostID]; ok {
		close(previous)
	}
	s.commands[hostID] = ch
	s.mu.Unlock()

	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		// Solo si sigue siendo el nuestro: si el host reconecto, este canal ya fue
		// reemplazado y cerrado, y borrar la entrada dejaria sin ordenes a la
		// conexion nueva.
		if current, ok := s.commands[hostID]; ok && current == ch {
			delete(s.commands, hostID)
			close(ch)
		}
	}
}

// Send le manda una orden a un host. Devuelve false si no tiene socket abierto o
// si no la esta leyendo.
//
// No bloquea nunca: un telefono colgado no puede frenar al que le esta pidiendo
// escuchar. Perder una orden se recupera solo, porque la reconciliacion vuelve a
// mandarla en la proxima vuelta.
func (s *Store) Send(hostID uuid.UUID, msg Message) bool {
	s.mu.RLock()
	ch, ok := s.commands[hostID]
	s.mu.RUnlock()

	if !ok {
		return false
	}

	select {
	case ch <- msg:
		return true
	default:
		return false
	}
}

// HasSocket dice si un host tiene el socket de presencia abierto, o sea si esta
// en condiciones de recibir ordenes.
func (s *Store) HasSocket(hostID uuid.UUID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.commands[hostID]
	return ok
}

// Watch devuelve un canal que recibe una senial cada vez que cambia quien esta
// activo o que esta sonando, y la funcion para darse de baja.
//
// El canal tiene buffer 1 y las seniales se **descartan** si ya hay una sin
// leer: dos cambios seguidos que el suscriptor todavia no miro son un solo
// "volve a mirar", y encolarlos solo lo haria recalcular de mas para llegar al
// mismo estado final.
func (s *Store) Watch() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)

	s.mu.Lock()
	s.watchers[ch] = struct{}{}
	s.mu.Unlock()

	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.watchers[ch]; ok {
			delete(s.watchers, ch)
			close(ch)
		}
	}
}

// notify avisa a los suscriptores. Hay que llamarla con el lock tomado.
func (s *Store) notify() {
	for ch := range s.watchers {
		select {
		case ch <- struct{}{}:
		default:
			// Ya tenia una senial pendiente. Ver Watch.
		}
	}
}

// Announce registra o refresca lo que esta sonando en el telefono de un host.
//
// Solo avisa a los suscriptores cuando cambio algo que se pueda ver: un host
// nuevo, o una cancion distinta. Un latido que repite la misma cancion refresca
// el vencimiento y no despierta a nadie — si no, cada host despertaria a todos
// sus amigos cada 15 segundos para no decirles nada, que es exactamente el
// trabajo que el socket venia a evitar.
func (s *Store) Announce(hostID uuid.UUID, username string, track Track) {
	if hostID == uuid.Nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	previous, existed := s.entries[hostID]
	changed := !existed || s.expired(previous) || previous.Track != track

	s.entries[hostID] = Entry{
		HostID:    hostID,
		Username:  username,
		Track:     track,
		UpdatedAt: s.now(),
	}

	if changed {
		s.notify()
	}
}

// Clear da de baja a un host. Se llama al cerrarse su socket, que es la salida
// limpia; el TTL cubre la sucia.
func (s *Store) Clear(hostID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, existed := s.entries[hostID]; !existed {
		return
	}
	delete(s.entries, hostID)
	s.notify()
}

// Sweep tira las entradas vencidas y avisa si saco alguna.
//
// Existe porque el vencimiento es la unica forma de dejar de estar activo que no
// tiene un evento detras: nadie llama a nada cuando a un telefono se le acaba la
// bateria, y el host que pausa simplemente deja de latir. Sin este barrido, el
// suscriptor se enteraria de que alguien se fue recien la proxima vez que otra
// cosa cualquiera provocara un recalculo.
//
// Devuelve cuantas saco, para los tests y los logs.
func (s *Store) Sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for hostID, entry := range s.entries {
		if s.expired(entry) {
			delete(s.entries, hostID)
			removed++
		}
	}
	if removed > 0 {
		s.notify()
	}
	return removed
}

// StartSweeper corre [Sweep] cada `every` hasta que se cancele el contexto.
func (s *Store) StartSweeper(ctx context.Context, every time.Duration) {
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.Sweep()
			}
		}
	}()
}

// Get devuelve el anuncio vivo de un host, si lo tiene y no vencio.
func (s *Store) Get(hostID uuid.UUID) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.entries[hostID]
	if !ok || s.expired(entry) {
		return Entry{}, false
	}
	return entry, true
}

// Active devuelve todos los anuncios vigentes.
//
// Aprovecha el recorrido para tirar los vencidos: sin un barrido periodico, el
// mapa creceria con cada host que se desconecto mal. Es el unico lugar que los
// recorre todos, asi que es donde la limpieza sale gratis.
func (s *Store) Active() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	active := make([]Entry, 0, len(s.entries))
	for hostID, entry := range s.entries {
		if s.expired(entry) {
			delete(s.entries, hostID)
			continue
		}
		active = append(active, entry)
	}
	return active
}

// Count son los hosts vigentes. Para logs y /health; no limpia.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	count := 0
	for _, entry := range s.entries {
		if !s.expired(entry) {
			count++
		}
	}
	return count
}

func (s *Store) expired(entry Entry) bool {
	return s.now().Sub(entry.UpdatedAt) > TTL
}
