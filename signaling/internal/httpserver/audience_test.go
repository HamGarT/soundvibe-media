package httpserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/soundvibe/media/signaling/internal/presence"
	"github.com/soundvibe/media/signaling/internal/rooms"
)

// fakeLister imita al SFU. Se usa en vez del real para poder representar lo que
// cuesta provocar de verdad: un room vacio, uno con gente, y el SFU caido.
type fakeLister struct {
	enabled bool
	byRoom  map[string][]string
	err     error
	calls   int
}

func (f *fakeLister) Enabled() bool { return f.enabled }

func (f *fakeLister) ListParticipantIdentities(_ context.Context, room string) ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.byRoom[room], nil
}

// recibir saca una orden del canal sin bloquear el test si no hay ninguna.
func recibir(commands <-chan presence.Message) presence.Command {
	// Los avisos de audiencia acompanian a las ordenes por el mismo canal; los
	// tests de aca miran las ordenes, asi que se saltean.
	for {
		select {
		case msg := <-commands:
			if msg.Type == presence.CommandListeners {
				continue
			}
			return msg.Type
		default:
			return ""
		}
	}
}

// recibirOyentes devuelve el primer aviso de audiencia que haya, si lo hay.
func recibirOyentes(commands <-chan presence.Message) ([]string, bool) {
	for {
		select {
		case msg := <-commands:
			if msg.Type == presence.CommandListeners {
				return msg.Listeners, true
			}
		default:
			return nil, false
		}
	}
}

func TestSePideAudioAlPrimerOyente(t *testing.T) {
	store := presence.NewStore()
	host := uuid.New()
	commands, detach := store.Attach(host)
	defer detach()

	a := newAudience(&fakeLister{enabled: true}, store)
	a.Requested(context.Background(), host, uuid.New())

	if cmd := recibir(commands); cmd != presence.CommandStartBroadcast {
		t.Fatalf("orden = %q, se esperaba %q", cmd, presence.CommandStartBroadcast)
	}
}

// Repetir la orden no aporta nada y el telefono ya esta transmitiendo.
func TestNoSeRepiteLaOrdenConMasOyentes(t *testing.T) {
	store := presence.NewStore()
	host := uuid.New()
	commands, detach := store.Attach(host)
	defer detach()

	a := newAudience(&fakeLister{enabled: true}, store)
	a.Requested(context.Background(), host, uuid.New())
	_ = recibir(commands)

	a.Requested(context.Background(), host, uuid.New())

	if cmd := recibir(commands); cmd != "" {
		t.Fatalf("no deberia haber una segunda orden, llego %q", cmd)
	}
}

// Un host que dejo de compartir entre que se dibujo la pantalla y el tap no
// tiene socket. No se lo puede marcar como transmitiendo, porque nunca se
// entero.
func TestHostSinSocketNoQuedaMarcado(t *testing.T) {
	store := presence.NewStore()
	host := uuid.New()

	a := newAudience(&fakeLister{enabled: true}, store)
	a.Requested(context.Background(), host, uuid.New())

	a.mu.Lock()
	_, serving := a.serving[host]
	a.mu.Unlock()

	if serving {
		t.Fatal("no se puede dar por transmitiendo a un host al que no se le pudo avisar")
	}
}

// Entre el token y la conexion hay un viaje de red, el handshake y el ICE. Sin
// gracia, la primera reconciliacion cortaria justo mientras el oyente entra.
func TestNoSeCortaDentroDeLaGracia(t *testing.T) {
	now := time.Now()
	store := presence.NewStore()
	host := uuid.New()
	commands, detach := store.Attach(host)
	defer detach()

	lister := &fakeLister{enabled: true, byRoom: map[string][]string{}}
	a := newAudience(lister, store)
	a.now = func() time.Time { return now }

	a.Requested(context.Background(), host, uuid.New())
	_ = recibir(commands)

	// El room esta vacio, pero el oyente todavia esta en camino.
	now = now.Add(audienceGrace - time.Second)
	a.Reconcile(context.Background())

	if cmd := recibir(commands); cmd != "" {
		t.Fatalf("no deberia cortar dentro de la gracia, llego %q", cmd)
	}
	if lister.calls != 0 {
		t.Fatalf("ni siquiera deberia preguntarle al SFU todavia (%d llamadas)", lister.calls)
	}
}

func TestSeCortaCuandoNoQuedaNadie(t *testing.T) {
	now := time.Now()
	store := presence.NewStore()
	host := uuid.New()
	commands, detach := store.Attach(host)
	defer detach()

	lister := &fakeLister{enabled: true, byRoom: map[string][]string{}}
	a := newAudience(lister, store)
	a.now = func() time.Time { return now }

	a.Requested(context.Background(), host, uuid.New())
	_ = recibir(commands)

	now = now.Add(audienceGrace + time.Second)
	a.Reconcile(context.Background())

	if cmd := recibir(commands); cmd != presence.CommandStopBroadcast {
		t.Fatalf("orden = %q, se esperaba %q", cmd, presence.CommandStopBroadcast)
	}
}

// El relay esta en el room con la identidad del host. Si contara como
// audiencia, ninguna transmision se apagaria jamas.
func TestElRelayNoCuentaComoAudiencia(t *testing.T) {
	now := time.Now()
	store := presence.NewStore()
	host := uuid.New()
	commands, detach := store.Attach(host)
	defer detach()

	lister := &fakeLister{
		enabled: true,
		byRoom: map[string][]string{
			// Solo el relay, publicando para nadie.
			rooms.Name(host): {host.String()},
		},
	}
	a := newAudience(lister, store)
	a.now = func() time.Time { return now }

	a.Requested(context.Background(), host, uuid.New())
	_ = recibir(commands)

	now = now.Add(audienceGrace + time.Second)
	a.Reconcile(context.Background())

	if cmd := recibir(commands); cmd != presence.CommandStopBroadcast {
		t.Fatalf("con solo el relay en el room hay que cortar, llego %q", cmd)
	}
}

func TestNoSeCortaConOyentesPresentes(t *testing.T) {
	now := time.Now()
	store := presence.NewStore()
	host := uuid.New()
	commands, detach := store.Attach(host)
	defer detach()

	lister := &fakeLister{
		enabled: true,
		byRoom: map[string][]string{
			rooms.Name(host): {host.String(), uuid.New().String()},
		},
	}
	a := newAudience(lister, store)
	a.now = func() time.Time { return now }

	a.Requested(context.Background(), host, uuid.New())
	_ = recibir(commands)

	now = now.Add(audienceGrace + time.Second)
	a.Reconcile(context.Background())

	if cmd := recibir(commands); cmd != "" {
		t.Fatalf("con un oyente presente no se corta, llego %q", cmd)
	}
}

// Cortar porque no se pudo preguntar seria cortarles a oyentes que quiza estan
// ahi. Ante la duda, sigue sonando.
func TestNoSeCortaSiElSFUNoResponde(t *testing.T) {
	now := time.Now()
	store := presence.NewStore()
	host := uuid.New()
	commands, detach := store.Attach(host)
	defer detach()

	lister := &fakeLister{enabled: true, err: errors.New("SFU caido")}
	a := newAudience(lister, store)
	a.now = func() time.Time { return now }

	a.Requested(context.Background(), host, uuid.New())
	_ = recibir(commands)

	now = now.Add(audienceGrace + time.Second)
	a.Reconcile(context.Background())

	if cmd := recibir(commands); cmd != "" {
		t.Fatalf("sin respuesta del SFU no se decide nada, llego %q", cmd)
	}
}

// Despues de cortar, el siguiente oyente tiene que volver a levantar la
// transmision: si el host quedara marcado, entraria a un room mudo.
func TestDespuesDeCortarSeVuelveAPedir(t *testing.T) {
	now := time.Now()
	store := presence.NewStore()
	host := uuid.New()
	commands, detach := store.Attach(host)
	defer detach()

	lister := &fakeLister{enabled: true, byRoom: map[string][]string{}}
	a := newAudience(lister, store)
	a.now = func() time.Time { return now }

	a.Requested(context.Background(), host, uuid.New())
	_ = recibir(commands)

	now = now.Add(audienceGrace + time.Second)
	a.Reconcile(context.Background())
	_ = recibir(commands)

	a.Requested(context.Background(), host, uuid.New())

	if cmd := recibir(commands); cmd != presence.CommandStartBroadcast {
		t.Fatalf("orden = %q, se esperaba volver a pedir audio", cmd)
	}
}

// El bug que hacia que compartir anduviera una sola vez: al dejar de compartir
// se limpiaba la presencia pero no este registro, asi que el host quedaba
// marcado como "ya transmitiendo" y al segundo intento no se le mandaba nada.
func TestSePuedeVolverACompartirDespuesDeCortar(t *testing.T) {
	store := presence.NewStore()
	host := uuid.New()

	// Primera sesion: comparte y alguien entra.
	primeras, detach := store.Attach(host)
	a := newAudience(&fakeLister{enabled: true}, store)
	a.Requested(context.Background(), host, uuid.New())
	if cmd := recibir(primeras); cmd != presence.CommandStartBroadcast {
		t.Fatalf("primera sesion: orden = %q", cmd)
	}

	// Deja de compartir: se cierra el socket.
	detach()

	// Sin olvidar, el host sigue marcado como atendido y el proximo oyente no
	// dispara nada. Esto es exactamente lo que se veia en el telefono: se apretaba
	// SHARE, alguien entraba, y el boton se quedaba en ON para siempre.
	sinOlvidar, detachSinOlvidar := store.Attach(host)
	a.Requested(context.Background(), host, uuid.New())
	if cmd := recibir(sinOlvidar); cmd != "" {
		t.Fatalf("sin Forget no deberia mandarse nada (es el bug), llego %q", cmd)
	}
	detachSinOlvidar()

	a.Forget(host)

	// Segunda sesion: vuelve a compartir y entra alguien otra vez.
	segundas, detach2 := store.Attach(host)
	defer detach2()
	a.Requested(context.Background(), host, uuid.New())

	if cmd := recibir(segundas); cmd != presence.CommandStartBroadcast {
		t.Fatalf("segunda sesion: orden = %q, se esperaba volver a pedir audio", cmd)
	}
}

// Aunque quede alguien en el room, cerrar el socket del host invalida lo que
// sabiamos: reconciliar no alcanzaba, porque ese oyente refrescaba el registro
// indefinidamente.
func TestForgetLimpiaAunqueQuedeAudiencia(t *testing.T) {
	store := presence.NewStore()
	host := uuid.New()
	commands, detach := store.Attach(host)
	defer detach()

	lister := &fakeLister{
		enabled: true,
		byRoom:  map[string][]string{rooms.Name(host): {host.String(), uuid.New().String()}},
	}
	a := newAudience(lister, store)
	a.Requested(context.Background(), host, uuid.New())
	_ = recibir(commands)

	a.Forget(host)

	a.mu.Lock()
	_, serving := a.serving[host]
	a.mu.Unlock()

	if serving {
		t.Fatal("Forget tiene que limpiar el registro haya o no gente en el room")
	}
}

// El host tiene que ver al oyente en el acto, no en la proxima reconciliacion:
// por eso la audiencia se anota en el join y no esperando a verla en el SFU.
func TestElHostSeEnteraDeSuAudienciaAlInstante(t *testing.T) {
	store := presence.NewStore()
	host := uuid.New()
	oyente := uuid.New()
	commands, detach := store.Attach(host)
	defer detach()

	a := newAudience(&fakeLister{enabled: true}, store)
	a.Requested(context.Background(), host, oyente)

	ids, ok := recibirOyentes(commands)
	if !ok {
		t.Fatal("tendria que haber llegado un aviso de audiencia")
	}
	if len(ids) != 1 || ids[0] != oyente.String() {
		t.Fatalf("audiencia = %v, se esperaba [%s]", ids, oyente)
	}
}

// El SFU manda sobre quien sigue conectado: lo anotado en el join es una
// intencion, y si el oyente ya no esta en el room, no esta.
func TestLaAudienciaSePodaConLoQueDiceElSFU(t *testing.T) {
	now := time.Now()
	store := presence.NewStore()
	host := uuid.New()
	seQueda := uuid.New()
	seFue := uuid.New()
	commands, detach := store.Attach(host)
	defer detach()

	lister := &fakeLister{
		enabled: true,
		byRoom: map[string][]string{
			rooms.Name(host): {host.String(), seQueda.String()},
		},
	}
	a := newAudience(lister, store)
	a.now = func() time.Time { return now }

	a.Requested(context.Background(), host, seQueda)
	a.Requested(context.Background(), host, seFue)
	// Se vacia lo anunciado hasta aca —cada join avisa la audiencia— para que lo
	// que quede sea lo que dijo la reconciliacion.
	_ = drenar(commands)

	now = now.Add(audienceGrace + time.Second)
	a.Reconcile(context.Background())

	ids, ok := ultimaAudiencia(drenar(commands))
	if !ok {
		t.Fatal("al cambiar la audiencia tendria que avisarse")
	}
	if len(ids) != 1 || ids[0] != seQueda.String() {
		t.Fatalf("audiencia = %v, se esperaba solo [%s]", ids, seQueda)
	}
}

// Sin LiveKit configurado no hay a quien preguntarle, asi que no se toma
// ninguna decision — en vez de dar por vacios todos los rooms.
func TestSinLiveKitNoSeReconcilia(t *testing.T) {
	store := presence.NewStore()
	host := uuid.New()
	commands, detach := store.Attach(host)
	defer detach()

	lister := &fakeLister{enabled: false}
	a := newAudience(lister, store)

	a.Requested(context.Background(), host, uuid.New())
	_ = recibir(commands)

	a.Reconcile(context.Background())

	if cmd := recibir(commands); cmd != "" {
		t.Fatalf("sin LiveKit no se corta nada, llego %q", cmd)
	}
}

// TestElUltimoOyenteQueSeVaCortaEnElActo es el motivo de que Left exista: sin
// el aviso, apagar la transmision depende de la reconciliacion, que corre cada
// diez segundos detras de una gracia de treinta — el host se queda en LIVE
// medio minuto largo despues de que ya no lo escucha nadie.
func TestElUltimoOyenteQueSeVaCortaEnElActo(t *testing.T) {
	store := presence.NewStore()
	host := uuid.New()
	oyente := uuid.New()
	commands, detach := store.Attach(host)
	defer detach()

	a := newAudience(&fakeLister{enabled: true}, store)
	a.Requested(context.Background(), host, oyente)
	_ = recibir(commands)

	a.Left(context.Background(), host, oyente)

	if cmd := recibir(commands); cmd != presence.CommandStopBroadcast {
		t.Fatalf("orden = %q, se esperaba %q", cmd, presence.CommandStopBroadcast)
	}

	// Y queda como si nunca hubiera transmitido, para que el proximo oyente
	// vuelva a pedirle audio en vez de darlo por atendido.
	a.mu.Lock()
	_, serving := a.serving[host]
	a.mu.Unlock()
	if serving {
		t.Error("el host sigue marcado como transmitiendo")
	}
}

func TestSiQuedanOyentesNoSeCortaAlIrseUno(t *testing.T) {
	store := presence.NewStore()
	host := uuid.New()
	seVa := uuid.New()
	seQueda := uuid.New()
	commands, detach := store.Attach(host)
	defer detach()

	a := newAudience(&fakeLister{enabled: true}, store)
	a.Requested(context.Background(), host, seVa)
	a.Requested(context.Background(), host, seQueda)
	_ = recibir(commands)

	a.Left(context.Background(), host, seVa)

	// Se mira todo lo que llego, no el primer mensaje: aca salen dos cosas —la
	// audiencia nueva y, si estuviera mal, la orden de parar— y los helpers de
	// arriba descartan lo que no buscan.
	mensajes := drenar(commands)
	for _, msg := range mensajes {
		if msg.Type == presence.CommandStopBroadcast {
			t.Fatal("se corto la transmision con un oyente todavia escuchando")
		}
	}

	// Al host se le dice quien queda, que es lo que dibuja las caras.
	ids, ok := ultimaAudiencia(mensajes)
	if !ok {
		t.Fatal("no se aviso la nueva audiencia")
	}
	if len(ids) != 1 || ids[0] != seQueda.String() {
		t.Errorf("audiencia = %v, se esperaba solo %s", ids, seQueda)
	}
}

// drenar saca todo lo que haya en el canal sin bloquear.
func drenar(commands <-chan presence.Message) []presence.Message {
	var out []presence.Message
	for {
		select {
		case msg := <-commands:
			out = append(out, msg)
		default:
			return out
		}
	}
}

// ultimaAudiencia devuelve el ultimo aviso de audiencia de la tanda, que es el
// que refleja el estado actual.
func ultimaAudiencia(mensajes []presence.Message) ([]string, bool) {
	ids, found := []string(nil), false
	for _, msg := range mensajes {
		if msg.Type == presence.CommandListeners {
			ids, found = msg.Listeners, true
		}
	}
	return ids, found
}

// Un aviso repetido —la app reintentando, o dos pantallas cerrandose— no puede
// cortarle la transmision a los que siguen escuchando.
func TestAvisoDeSalidaRepetidoNoCorta(t *testing.T) {
	store := presence.NewStore()
	host := uuid.New()
	seVa := uuid.New()
	seQueda := uuid.New()
	commands, detach := store.Attach(host)
	defer detach()

	a := newAudience(&fakeLister{enabled: true}, store)
	a.Requested(context.Background(), host, seVa)
	a.Requested(context.Background(), host, seQueda)
	_ = recibir(commands)

	a.Left(context.Background(), host, seVa)
	a.Left(context.Background(), host, seVa)

	if cmd := recibir(commands); cmd == presence.CommandStopBroadcast {
		t.Fatal("el aviso repetido corto la transmision")
	}
}

// Irse de un host al que no se estaba escuchando no toca nada de ese host.
func TestSalidaDeQuienNoEstabaEscuchandoNoAfecta(t *testing.T) {
	store := presence.NewStore()
	host := uuid.New()
	oyente := uuid.New()
	commands, detach := store.Attach(host)
	defer detach()

	a := newAudience(&fakeLister{enabled: true}, store)
	a.Requested(context.Background(), host, oyente)
	_ = recibir(commands)

	a.Left(context.Background(), host, uuid.New())

	if cmd := recibir(commands); cmd == presence.CommandStopBroadcast {
		t.Fatal("un desconocido corto la transmision de otro")
	}
	a.mu.Lock()
	_, serving := a.serving[host]
	a.mu.Unlock()
	if !serving {
		t.Error("el host dejo de estar marcado como transmitiendo")
	}
}

// TestElSegundoOyenteTambienSeAvisa cubre lo que el test de arriba destapo: al
// segundo oyente no se lo reportaba nunca. La orden de transmitir no se repite
// —el host ya esta transmitiendo— pero la audiencia si cambio, y la
// reconciliacion no lo iba a arreglar: compara lo que ve el SFU con lo anotado,
// y ya coincidian.
func TestElSegundoOyenteTambienSeAvisa(t *testing.T) {
	store := presence.NewStore()
	host := uuid.New()
	primero := uuid.New()
	segundo := uuid.New()
	commands, detach := store.Attach(host)
	defer detach()

	a := newAudience(&fakeLister{enabled: true}, store)
	a.Requested(context.Background(), host, primero)
	a.Requested(context.Background(), host, segundo)

	ids, ok := ultimaAudiencia(drenar(commands))
	if !ok {
		t.Fatal("no se aviso la audiencia")
	}
	if len(ids) != 2 {
		t.Errorf("audiencia = %v, se esperaban los dos oyentes", ids)
	}
}
