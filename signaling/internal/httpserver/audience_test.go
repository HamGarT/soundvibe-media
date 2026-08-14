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
func recibir(commands <-chan presence.Command) presence.Command {
	select {
	case cmd := <-commands:
		return cmd
	default:
		return ""
	}
}

func TestSePideAudioAlPrimerOyente(t *testing.T) {
	store := presence.NewStore()
	host := uuid.New()
	commands, detach := store.Attach(host)
	defer detach()

	a := newAudience(&fakeLister{enabled: true}, store)
	a.Requested(context.Background(), host)

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
	a.Requested(context.Background(), host)
	_ = recibir(commands)

	a.Requested(context.Background(), host)

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
	a.Requested(context.Background(), host)

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

	a.Requested(context.Background(), host)
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

	a.Requested(context.Background(), host)
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

	a.Requested(context.Background(), host)
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

	a.Requested(context.Background(), host)
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

	a.Requested(context.Background(), host)
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

	a.Requested(context.Background(), host)
	_ = recibir(commands)

	now = now.Add(audienceGrace + time.Second)
	a.Reconcile(context.Background())
	_ = recibir(commands)

	a.Requested(context.Background(), host)

	if cmd := recibir(commands); cmd != presence.CommandStartBroadcast {
		t.Fatalf("orden = %q, se esperaba volver a pedir audio", cmd)
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

	a.Requested(context.Background(), host)
	_ = recibir(commands)

	a.Reconcile(context.Background())

	if cmd := recibir(commands); cmd != "" {
		t.Fatalf("sin LiveKit no se corta nada, llego %q", cmd)
	}
}
