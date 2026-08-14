package httpserver

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/soundvibe/media/signaling/internal/presence"
	"github.com/soundvibe/media/signaling/internal/rooms"
)

const (
	// audienceReconcile es cada cuanto se comprueba quien sigue realmente en cada
	// room.
	//
	// El unico evento fiable que tiene este servicio es la entrada: /rooms/join
	// pasa por aca. La salida no — un telefono que se queda sin bateria no avisa
	// nada —, asi que la fuente de verdad de "todavia hay alguien escuchando" es
	// preguntarle al SFU, que es el que tiene las conexiones.
	audienceReconcile = 10 * time.Second

	// audienceGrace es cuanto se le da a un oyente para aparecer en el room
	// despues de pedir el token.
	//
	// Entre el token y la conexion hay un viaje de red, el handshake con el SFU y
	// el ICE. Sin esta gracia, la primera reconciliacion veria el room vacio y le
	// diria al host que pare justo mientras el oyente estaba entrando — y el
	// oyente caeria en un room mudo.
	audienceGrace = 30 * time.Second
)

// audience lleva la cuenta de a que hosts se les pidio audio y si todavia hace
// falta.
//
// Existe por la decision 2 del plan: el host no publica de forma continuo, sino
// cuando alguien lo quiere escuchar. Para eso hacen falta las dos puntas — saber
// cuando arrancar (una entrada) y cuando parar (que no quede nadie) — y la
// segunda no tiene evento propio.
type audience struct {
	livekit  livekitLister
	presence *presence.Store

	mu      sync.Mutex
	wanted  map[uuid.UUID]time.Time
	serving map[uuid.UUID]struct{}

	now func() time.Time
}

// livekitLister es lo unico que audience necesita del SFU. Se declara aca, del
// lado del que consume, para poder sustituirlo en los tests sin levantar un
// LiveKit.
type livekitLister interface {
	Enabled() bool
	ListParticipantIdentities(ctx context.Context, room string) ([]string, error)
}

func newAudience(lister livekitLister, store *presence.Store) *audience {
	return &audience{
		livekit:  lister,
		presence: store,
		wanted:   make(map[uuid.UUID]time.Time),
		serving:  make(map[uuid.UUID]struct{}),
		now:      time.Now,
	}
}

// Requested registra que alguien pidio escuchar a un host, y se lo dice si es el
// primero.
//
// Se llama al firmar un token de oyente, no al conectarse este: es el ultimo
// momento en que este servicio se entera de algo, y el audio tiene que estar
// saliendo para cuando el oyente termine de entrar al room.
func (a *audience) Requested(ctx context.Context, hostID uuid.UUID) {
	a.mu.Lock()
	a.wanted[hostID] = a.now()
	_, alreadyServing := a.serving[hostID]
	if !alreadyServing {
		a.serving[hostID] = struct{}{}
	}
	a.mu.Unlock()

	if alreadyServing {
		// Ya le habiamos dicho que transmita y sigue habiendo gente. Repetirselo no
		// aporta nada.
		return
	}

	if !a.presence.Send(hostID, presence.CommandStartBroadcast) {
		// Sin socket de presencia no hay forma de avisarle. Pasa con un host que
		// dejo de compartir entre que se dibujo la pantalla del oyente y el tap;
		// el oyente entra a un room mudo y su propia maquina de estados lo cuenta.
		slog.WarnContext(ctx, "no se le pudo pedir audio al host: no tiene socket de presencia",
			"host", hostID)
		a.mu.Lock()
		delete(a.serving, hostID)
		delete(a.wanted, hostID)
		a.mu.Unlock()
		return
	}

	slog.InfoContext(ctx, "audio pedido al host", "host", hostID)
}

// Reconcile pregunta al SFU quien sigue conectado y para las transmisiones que
// ya no escucha nadie.
func (a *audience) Reconcile(ctx context.Context) {
	if !a.livekit.Enabled() {
		return
	}

	a.mu.Lock()
	hosts := make([]uuid.UUID, 0, len(a.serving))
	for hostID := range a.serving {
		hosts = append(hosts, hostID)
	}
	a.mu.Unlock()

	for _, hostID := range hosts {
		a.reconcileHost(ctx, hostID)
	}
}

func (a *audience) reconcileHost(ctx context.Context, hostID uuid.UUID) {
	a.mu.Lock()
	requestedAt, wanted := a.wanted[hostID]
	a.mu.Unlock()

	// Todavia dentro de la ventana en la que el oyente puede estar entrando.
	if wanted && a.now().Sub(requestedAt) < audienceGrace {
		return
	}

	identities, err := a.livekit.ListParticipantIdentities(ctx, rooms.Name(hostID))
	if err != nil {
		// Sin respuesta del SFU no se decide nada. Cortar una transmision porque
		// no se pudo preguntar seria cortarsela a oyentes que quiza esten ahi.
		slog.WarnContext(ctx, "no se pudo comprobar la audiencia",
			"host", hostID, "error", err)
		return
	}

	// El relay esta en el room con la identidad del host, asi que no cuenta como
	// audiencia: si contara, ninguna transmision se apagaria nunca.
	listeners := 0
	for _, identity := range identities {
		if identity != hostID.String() {
			listeners++
		}
	}

	if listeners > 0 {
		a.mu.Lock()
		a.wanted[hostID] = a.now()
		a.mu.Unlock()
		return
	}

	a.mu.Lock()
	delete(a.serving, hostID)
	delete(a.wanted, hostID)
	a.mu.Unlock()

	a.presence.Send(hostID, presence.CommandStopBroadcast)
	slog.InfoContext(ctx, "sin oyentes: se le pidio al host que pare", "host", hostID)
}

// StartReconciler corre [Reconcile] cada `every` hasta que se cancele el
// contexto.
func (a *audience) StartReconciler(ctx context.Context, every time.Duration) {
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.Reconcile(ctx)
			}
		}
	}()
}
