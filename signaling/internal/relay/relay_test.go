package relay

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// Abrir una sesion de verdad necesita un SFU al otro lado, asi que lo que se
// cubre aca es el registro de quien esta transmitiendo — que es de donde sale la
// respuesta de "amigos activos" — y el cierre de sesiones.
//
// Las sesiones de prueba se insertan ya cerradas: Close() corta antes de tocar
// el track y el room, con lo que no hace falta ni conexion ni dobles.

func closedSession(hostID uuid.UUID) *Session {
	return &Session{hostID: hostID, closed: true}
}

func (r *Relay) putSession(session *Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[session.hostID] = session
}

func newTestRelay() *Relay {
	return &Relay{sessions: make(map[uuid.UUID]*Session)}
}

func TestIsBroadcasting(t *testing.T) {
	r := newTestRelay()
	transmitiendo := uuid.New()
	callado := uuid.New()
	r.putSession(closedSession(transmitiendo))

	if !r.IsBroadcasting(transmitiendo) {
		t.Error("el host con sesion abierta deberia figurar como transmitiendo")
	}
	if r.IsBroadcasting(callado) {
		t.Error("un host sin sesion no deberia figurar como transmitiendo")
	}
}

func TestBroadcastingListsEveryHost(t *testing.T) {
	r := newTestRelay()
	first := uuid.New()
	second := uuid.New()
	r.putSession(closedSession(first))
	r.putSession(closedSession(second))

	hosts := r.Broadcasting()
	if len(hosts) != 2 {
		t.Fatalf("hosts = %d, se esperaban 2", len(hosts))
	}

	found := map[uuid.UUID]bool{}
	for _, hostID := range hosts {
		found[hostID] = true
	}
	if !found[first] || !found[second] {
		t.Errorf("faltan hosts en %v", hosts)
	}
}

func TestBroadcastingIsEmptyWithoutSessions(t *testing.T) {
	// Devolver un slice vacio y no nil le ahorra a quien llama distinguir los dos
	// casos al serializar.
	hosts := newTestRelay().Broadcasting()
	if hosts == nil {
		t.Fatal("Broadcasting devolvio nil, se esperaba un slice vacio")
	}
	if len(hosts) != 0 {
		t.Errorf("hosts = %d, se esperaban 0", len(hosts))
	}
}

func TestStopRemovesTheSession(t *testing.T) {
	r := newTestRelay()
	hostID := uuid.New()
	r.putSession(closedSession(hostID))

	r.Stop(hostID)

	if r.IsBroadcasting(hostID) {
		t.Error("despues de Stop el host no deberia figurar como transmitiendo")
	}
}

func TestStopOnAnUnknownHostIsHarmless(t *testing.T) {
	// Pasa cuando el WebSocket se cae despues de que la sesion ya se cerro por
	// otro camino; no tiene por que ser un error.
	newTestRelay().Stop(uuid.New())
}

func TestCloseAllEmptiesTheRegistry(t *testing.T) {
	r := newTestRelay()
	r.putSession(closedSession(uuid.New()))
	r.putSession(closedSession(uuid.New()))

	r.CloseAll()

	if hosts := r.Broadcasting(); len(hosts) != 0 {
		t.Errorf("quedaron %d sesiones despues de CloseAll", len(hosts))
	}
}

func TestWriteFrameOnAClosedSession(t *testing.T) {
	session := closedSession(uuid.New())

	err := session.WriteFrame([]byte{1, 2, 3})

	if !errors.Is(err, ErrSessionClosed) {
		t.Errorf("err = %v, se esperaba ErrSessionClosed", err)
	}
}

func TestWriteFrameIgnoresEmptyFrames(t *testing.T) {
	// Se descarta antes de mirar el estado de la sesion: un frame vacio no es
	// audio y no tiene sentido reenviarlo ni contarlo.
	session := closedSession(uuid.New())

	if err := session.WriteFrame(nil); err != nil {
		t.Errorf("un frame vacio no deberia dar error, dio %v", err)
	}
	if frames := session.Frames(); frames != 0 {
		t.Errorf("frames = %d, se esperaba 0", frames)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	// El cierre llega por dos caminos que pueden cruzarse: el defer del handler
	// del WebSocket y el reemplazo de sesion cuando el host reconecta.
	session := closedSession(uuid.New())
	session.Close()
	session.Close()
}
