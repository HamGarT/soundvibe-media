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
