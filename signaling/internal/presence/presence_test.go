package presence

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// clock devuelve un reloj que el test mueve a mano, para poder hacer vencer
// entradas sin dormir el TTL entero.
func clock(at *time.Time) func() time.Time {
	return func() time.Time { return *at }
}

func TestAnnounceYGet(t *testing.T) {
	store := NewStore()
	host := uuid.New()

	store.Announce(host, "ana", Track{Title: "Otra Vez", Artist: "Nova"})

	entry, ok := store.Get(host)
	if !ok {
		t.Fatal("el host recien anunciado deberia estar activo")
	}
	if entry.Track.Title != "Otra Vez" || entry.Track.Artist != "Nova" {
		t.Fatalf("se guardo otra cosa: %+v", entry.Track)
	}
	if entry.Username != "ana" {
		t.Fatalf("username = %q, se esperaba \"ana\"", entry.Username)
	}
}

func TestAnnounceReemplazaElAnterior(t *testing.T) {
	store := NewStore()
	host := uuid.New()

	store.Announce(host, "ana", Track{Title: "Primera"})
	store.Announce(host, "ana", Track{Title: "Segunda"})

	entry, _ := store.Get(host)
	if entry.Track.Title != "Segunda" {
		t.Fatalf("title = %q, se esperaba la cancion nueva", entry.Track.Title)
	}
	if len(store.Active()) != 1 {
		t.Fatalf("un host que cambia de cancion sigue siendo un solo host activo")
	}
}

func TestHostNiloSeIgnora(t *testing.T) {
	store := NewStore()
	store.Announce(uuid.Nil, "nadie", Track{Title: "x"})

	if len(store.Active()) != 0 {
		t.Fatal("un hostID nil no deberia entrar al store")
	}
}

func TestClearDaDeBaja(t *testing.T) {
	store := NewStore()
	host := uuid.New()

	store.Announce(host, "ana", Track{Title: "Otra Vez"})
	store.Clear(host)

	if _, ok := store.Get(host); ok {
		t.Fatal("despues de Clear el host no deberia estar activo")
	}
}

// Es el caso que motiva el TTL: el telefono pierde la red y nunca cierra el
// socket, asi que nadie llama a Clear.
func TestEntradaVencidaNoEstaActiva(t *testing.T) {
	now := time.Now()
	store := NewStore()
	store.now = clock(&now)

	host := uuid.New()
	store.Announce(host, "ana", Track{Title: "Otra Vez"})

	now = now.Add(TTL + time.Second)

	if _, ok := store.Get(host); ok {
		t.Fatal("una entrada mas vieja que el TTL deberia estar vencida")
	}
	if len(store.Active()) != 0 {
		t.Fatal("Active no deberia devolver entradas vencidas")
	}
	if store.Count() != 0 {
		t.Fatalf("Count = %d, se esperaba 0", store.Count())
	}
}

func TestLatidoMantieneVivaLaEntrada(t *testing.T) {
	now := time.Now()
	store := NewStore()
	store.now = clock(&now)

	host := uuid.New()
	store.Announce(host, "ana", Track{Title: "Otra Vez"})

	// Dos intervalos de latido: mas de lo que el cliente espera entre mensajes,
	// bastante menos que el TTL.
	for range 2 {
		now = now.Add(Heartbeat)
		store.Announce(host, "ana", Track{Title: "Otra Vez"})
	}
	now = now.Add(Heartbeat)

	if _, ok := store.Get(host); !ok {
		t.Fatal("un host que sigue latiendo tiene que seguir activo")
	}
}

// Active es el unico recorrido completo del mapa, asi que es donde se limpia:
// sin esto el store crece con cada host que se desconecto mal.
func TestActiveLimpiaLasVencidas(t *testing.T) {
	now := time.Now()
	store := NewStore()
	store.now = clock(&now)

	ido := uuid.New()
	store.Announce(ido, "ana", Track{Title: "Vieja"})

	now = now.Add(TTL + time.Second)

	presente := uuid.New()
	store.Announce(presente, "beto", Track{Title: "Nueva"})

	active := store.Active()
	if len(active) != 1 || active[0].HostID != presente {
		t.Fatalf("Active deberia devolver solo al host vigente, devolvio %+v", active)
	}
	if _, sigue := store.entries[ido]; sigue {
		t.Fatal("la entrada vencida deberia haberse borrado del mapa")
	}
}

// notificado dice si llego una senial, sin bloquear el test si no llega.
func notificado(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestWatchAvisaCuandoApareceUnHost(t *testing.T) {
	store := NewStore()
	changes, unwatch := store.Watch()
	defer unwatch()

	store.Announce(uuid.New(), "ana", Track{Title: "Otra Vez"})

	if !notificado(changes) {
		t.Fatal("un host nuevo tendria que despertar a los suscriptores")
	}
}

func TestWatchAvisaCuandoCambiaLaCancion(t *testing.T) {
	store := NewStore()
	host := uuid.New()
	store.Announce(host, "ana", Track{Title: "Primera"})

	changes, unwatch := store.Watch()
	defer unwatch()

	store.Announce(host, "ana", Track{Title: "Segunda"})

	if !notificado(changes) {
		t.Fatal("cambiar de cancion tendria que despertar a los suscriptores")
	}
}

// Es la regla que hace que el socket valga la pena: si cada latido despertara a
// todos los amigos del host, se estaria haciendo el mismo trabajo que el sondeo
// que se vino a sacar, pero peor repartido.
func TestLatidoRepetidoNoAvisa(t *testing.T) {
	store := NewStore()
	host := uuid.New()
	track := Track{Title: "Otra Vez", Artist: "Nova"}
	store.Announce(host, "ana", track)

	changes, unwatch := store.Watch()
	defer unwatch()

	store.Announce(host, "ana", track)
	store.Announce(host, "ana", track)

	if notificado(changes) {
		t.Fatal("repetir la misma cancion no deberia despertar a nadie")
	}
}

// La posicion viaja dentro de Track, asi que avanzar en la cancion cuenta como
// cambio. Es deseable — es lo que mantiene viva la barra de progreso — pero hay
// que saberlo, porque significa que un host que late con posicion nueva si
// despierta a sus amigos.
func TestAvanzarLaPosicionCuentaComoCambio(t *testing.T) {
	store := NewStore()
	host := uuid.New()
	store.Announce(host, "ana", Track{Title: "Otra Vez", PositionMs: 1000})

	changes, unwatch := store.Watch()
	defer unwatch()

	store.Announce(host, "ana", Track{Title: "Otra Vez", PositionMs: 16000})

	if !notificado(changes) {
		t.Fatal("una posicion distinta es un cambio visible")
	}
}

func TestClearAvisa(t *testing.T) {
	store := NewStore()
	host := uuid.New()
	store.Announce(host, "ana", Track{Title: "Otra Vez"})

	changes, unwatch := store.Watch()
	defer unwatch()

	store.Clear(host)

	if !notificado(changes) {
		t.Fatal("dar de baja a un host tendria que despertar a los suscriptores")
	}
}

func TestClearDeAlguienQueNoEstabaNoAvisa(t *testing.T) {
	store := NewStore()
	changes, unwatch := store.Watch()
	defer unwatch()

	store.Clear(uuid.New())

	if notificado(changes) {
		t.Fatal("borrar algo que no estaba no cambia nada")
	}
}

// Vencer es la unica forma de dejar de estar activo que no tiene un evento
// detras. Sin el barrido, el suscriptor se enteraria recien cuando otra cosa
// cualquiera provocara un recalculo.
func TestSweepAvisaCuandoVenceAlguien(t *testing.T) {
	now := time.Now()
	store := NewStore()
	store.now = clock(&now)

	host := uuid.New()
	store.Announce(host, "ana", Track{Title: "Otra Vez"})

	changes, unwatch := store.Watch()
	defer unwatch()

	now = now.Add(TTL + time.Second)

	if removed := store.Sweep(); removed != 1 {
		t.Fatalf("Sweep saco %d entradas, se esperaba 1", removed)
	}
	if !notificado(changes) {
		t.Fatal("que alguien venza tendria que despertar a los suscriptores")
	}
}

func TestSweepSinVencidasNoAvisa(t *testing.T) {
	store := NewStore()
	store.Announce(uuid.New(), "ana", Track{Title: "Otra Vez"})

	changes, unwatch := store.Watch()
	defer unwatch()

	if removed := store.Sweep(); removed != 0 {
		t.Fatalf("Sweep saco %d entradas, se esperaba 0", removed)
	}
	if notificado(changes) {
		t.Fatal("un barrido que no saco nada no deberia despertar a nadie")
	}
}

// El canal tiene buffer 1: dos cambios que el suscriptor todavia no miro son un
// solo "volve a mirar", no dos.
func TestLasSenialesSeAgrupan(t *testing.T) {
	store := NewStore()
	changes, unwatch := store.Watch()
	defer unwatch()

	store.Announce(uuid.New(), "ana", Track{Title: "Una"})
	store.Announce(uuid.New(), "beto", Track{Title: "Otra"})
	store.Announce(uuid.New(), "cata", Track{Title: "Y Otra"})

	if !notificado(changes) {
		t.Fatal("tendria que haber al menos una senial")
	}
	if notificado(changes) {
		t.Fatal("las seniales sin leer tendrian que agruparse en una sola")
	}
}

// Un suscriptor que se va no puede frenar a los que quedan: si su canal se
// llenara y el notify bloqueara, un telefono que se colgo dejaria sin
// actualizaciones a todos los demas.
func TestDarseDeBajaNoFrenaALosDemas(t *testing.T) {
	store := NewStore()

	abandonado, _ := store.Watch()
	_ = abandonado // nunca se lee: simula un cliente colgado

	activo, unwatch := store.Watch()
	defer unwatch()

	for range 5 {
		store.Announce(uuid.New(), "ana", Track{Title: "Otra Vez"})
	}

	if !notificado(activo) {
		t.Fatal("un suscriptor que no lee no deberia dejar sin avisos a los demas")
	}
}

func TestUnwatchEsIdempotente(t *testing.T) {
	store := NewStore()
	_, unwatch := store.Watch()

	unwatch()
	unwatch()

	// Y despues de darse de baja, anunciar no tiene que entrar en panico por
	// escribir en un canal cerrado.
	store.Announce(uuid.New(), "ana", Track{Title: "Otra Vez"})
}

func TestUsoConcurrente(t *testing.T) {
	store := NewStore()
	host := uuid.New()
	done := make(chan struct{})

	go func() {
		defer close(done)
		for range 500 {
			store.Announce(host, "ana", Track{Title: "Otra Vez"})
		}
	}()
	for range 500 {
		store.Active()
		store.Count()
	}
	<-done
}
