# soundvibe-media

SFU de audio (LiveKit) + servicio de **signaling** de SoundVibe.

`signaling` es el portero entre la app y LiveKit. No maneja audio (eso es
LiveKit) ni usuarios (eso es [soundvibe-core](../soundvibe-core)): recibe
"quiero entrar al room de X", le pregunta a core **quien es** el usuario y **si
tiene permiso**, y solo entonces firma un token de LiveKit.

No tiene base de datos. Es un servicio sin estado, a proposito.

## El flujo completo

```
app → signaling            POST /rooms/join { host_id }
                           Authorization: Bearer <access token de core>

signaling → core           POST /internal/introspect      → quien es el usuario
signaling → core           GET  /internal/listening-permission → ¿puede?

           200 → signaling firma el token de LiveKit y lo devuelve
           403 → listening_not_allowed, sin token
     sin respuesta → 503 core_unavailable, sin token
```

**Regla de oro: fallar cerrado.** Cualquier resultado que no sea un 200
explicito de core — 403, 500, timeout, DNS caido, JSON corrupto, incluso un 200
con `allowed:false` — se trata como denegacion. Un error de red nunca termina en
un permiso concedido. Esto esta cubierto por tests.

## Stack

| Pieza | Eleccion |
|---|---|
| SFU | `livekit/livekit-server` v1.8, un solo nodo, sin Redis |
| Signaling | Go 1.26, `net/http` + `chi`, binario estatico en alpine |
| Tokens de LiveKit | `github.com/livekit/protocol/auth` (los tipos del vendor, no claims a mano) |
| TLS | Caddy con Let's Encrypt automatico (perfil opcional) |

## Estructura

```
livekit.yaml              config del SFU (sin secretos)
Caddyfile                 reverse proxy TLS, dominios desde el entorno
docker-compose.yml        livekit + signaling + caddy
signaling/
  Dockerfile
  cmd/signaling/          punto de entrada
  internal/
    config/               carga y validacion de env vars
    core/                 cliente HTTP de soundvibe-core (falla cerrado)
    rooms/                nombre del room y firma de tokens de LiveKit
    httpserver/           router, handler de join, health
```

## Puesta en marcha

Para desplegar en el VPS, seguir el runbook completo en
**[soundvibe-core/DEPLOY.md](../soundvibe-core/DEPLOY.md)**, que cubre los dos
stacks en orden, TLS incluido.

Para desarrollo local, lo que sigue. Requisitos: Docker. **El stack de
soundvibe-core tiene que estar levantado primero** — este compose se engancha a
su red para resolver `sv-core-api`.

```bash
# 1. core primero
cd ../soundvibe-core && make up && make migrate-up

# 2. este stack
cd ../soundvibe-media
cp .env.example .env
# INTERNAL_API_KEY debe ser IDENTICA a la del .env de soundvibe-core
openssl rand -base64 48   # para LIVEKIT_API_SECRET

make up
curl localhost:8092/health   # {"status":"ok","core":"ok"}
```

Con TLS y dominios reales (requiere DNS ya apuntando al VPS):

```bash
make up-proxy
```

### Comandos utiles

```bash
make help          # lista todos los targets
make test          # suite completa; core se simula, no hace falta levantarlo
make logs          # logs de signaling
make logs-livekit  # logs del SFU
```

## API

### `POST /rooms/join`

Requiere `Authorization: Bearer <access token de soundvibe-core>`.

```jsonc
// body (opcional)
{ "host_id": "uuid del host que se quiere escuchar" }
// sin body, o sin host_id: el usuario abre SU PROPIO room como host
```

Respuesta 200:

```json
{
  "livekit_url": "wss://livekit.tudominio.com",
  "token": "<jwt de livekit>",
  "room": "listen:<host_uuid>",
  "identity": "<uuid del usuario>",
  "role": "listener",
  "expires_at": "2026-08-11T22:13:30Z"
}
```

| Status | Codigo | Que paso |
|---|---|---|
| 200 | — | Token emitido |
| 400 | `bad_request` | `host_id` no es un UUID, o el body no es JSON |
| 401 | `unauthorized` | Falta el access token, o es invalido/expiro |
| 403 | `listening_not_allowed` | core dijo que no |
| 503 | `core_unavailable` | No se pudo consultar a core: se deniega |

El cliente conecta a `livekit_url` con `token` usando el SDK de LiveKit. El
token dura 10 minutos por defecto y solo hace falta para **entrar** al room.

### `GET /health`

Sin auth. Responde 200 siempre que el proceso este vivo, e informa aparte si core
es alcanzable:

```json
{ "status": "degraded", "core": "unreachable" }
```

Devuelve 200 aun con core caido a proposito: el proceso esta sano y reiniciarlo
no arreglaria nada.

## Convenciones compartidas con el cliente

Cambiar cualquiera de estas dos rompe apps ya publicadas:

- **Nombre del room:** `listen:<uuid del host>`. Un room por host, no por sesion.
- **`identity` de LiveKit:** el UUID del usuario en core, **no** el username. El
  username puede cambiar; el UUID es lo que entiende el endpoint de permisos.

## Los grants son la barrera real

La consulta a core decide si se **entrega** un token. Una vez que el cliente esta
dentro del room, lo unico que lo limita son los grants del token:

| Rol | canPublish | canSubscribe | canPublishData |
|---|---|---|---|
| host | ✅ | ❌ | ❌ |
| listener | ❌ | ✅ | ❌ |

Ningun rol recibe `roomAdmin`, `roomCreate` ni `roomList`: un cliente no puede
expulsar participantes ni descubrir quien mas esta transmitiendo.

⚠️ **En LiveKit, un `canPublish` ausente equivale a `true`.** Por eso se usan los
tipos del SDK oficial en vez de armar los claims a mano, y por eso hay un test
que falla si el campo desaparece del token. Un oyente que pueda publicar mete
audio en el room de otro.

## Red y puertos

| Puerto | Protocolo | Para que |
|---|---|---|
| 7880 | TCP | API/WebSocket de LiveKit (detras de Caddy en produccion) |
| 7881 | TCP | Fallback ICE para clientes en redes que bloquean UDP |
| 50000-50200 | **UDP** | Trafico de media. **No pasa por Caddy**: abrir en el firewall |
| 8092 | TCP | signaling (detras de Caddy en produccion) |

`network_mode: host` en el contenedor de LiveKit **no es opcional en Linux**: el
proxy de userland de Docker degrada el rango UDP de media.

En Docker Desktop (Windows/Mac) `network_mode: host` se ignora y el SFU no queda
alcanzable desde fuera de la maquina. Para desarrollar el flujo de permisos no
importa, porque signaling no habla con LiveKit — solo firma tokens.

Capacidad: audio son ~50 kbps por stream, asi que los 200 Mbit/s del VPS dan de
sobra; el limite real es el CPU del SFU.

## Tests

```bash
make test
```

No necesita servicios levantados: core se simula con un `httptest.Server`, que es
la unica forma de provocar lo que importa — core caido, JSON corrupto, la API key
mal configurada.

Lo que se verifica:

- Los grants en el token firmado, decodificando el JWT y mirando el claim `video`
  tal como lo vera LiveKit.
- Que el token valide contra el `LIVEKIT_API_SECRET` y no contra otro.
- Que `identity` sea el UUID y no el username.
- Fallar cerrado en las cinco formas en que core puede fallar.
- Que un 401 por **nuestra API key** se reporte como 503 y no como 401: mandar al
  usuario a reautenticarse cuando el problema es de configuracion del servidor
  manda a perseguir el bug equivocado.

## Verificado end to end

Con los dos stacks levantados de verdad:

| Escenario | Resultado |
|---|---|
| No amigos → room de otro | 403, sin token |
| Host abre su propio room | 200, `role=host`, `canPublish:true` |
| Amigos → room del host | 200, `role=listener`, `canPublish:false` |
| Host excluye al oyente | 403, sin token |
| Access token invalido | 401 |
| **core apagado** | 503, sin token, y `/health` marca `degraded` |
| core vuelve | recuperacion automatica en ~1s, sin reiniciar signaling |

## Pendiente: revocacion en vivo

Si un host apaga el compartir **mientras** alguien lo escucha, no pasa nada: el
token de LiveKit ya fue emitido y el participante sigue conectado. El permiso
solo se consulta al entrar.

Opciones, cuando se decida atacarlo:

1. Que el cliente renueve el token cada pocos minutos y signaling reconsulte a
   core (simple, ventana de exposicion = TTL del token).
2. Que signaling llame a `RemoveParticipant` de la API de LiveKit cuando cambian
   los permisos, lo que implica que core avise a signaling en `PATCH /users/me` y
   al tocar excepciones (inmediato, mas piezas moviles).

Vale decidirlo antes de publicar: "apague el compartir y me siguieron
escuchando" se percibe como una traicion, no como un bug.

## Fuera de alcance

Usuarios, amistades y permisos (viven en `soundvibe-core`), cualquier cosa de
Combiyki, y grabacion de sesiones.
