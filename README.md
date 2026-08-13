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
// body (obligatorio)
{ "host_id": "uuid del host que se quiere escuchar" }
```

Solo emite tokens de **oyente**. Transmitir no se pide por aca: el host abre el
WebSocket de `GET /rooms/broadcast` y el que entra al room a publicar es este
servicio, con la identidad del host.

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
| 400 | `bad_request` | Falta `host_id`, no es un UUID, o el body no es JSON |
| 401 | `unauthorized` | Falta el access token, o es invalido/expiro |
| 403 | `listening_not_allowed` | core dijo que no |
| 409 | `own_room` | Es tu propio room: ahi ya esta el relay con tu identidad |
| 503 | `core_unavailable` | No se pudo consultar a core: se deniega |

El cliente conecta a `livekit_url` con `token` usando el SDK de LiveKit. El
token dura 10 minutos por defecto y solo hace falta para **entrar** al room.

### `GET /rooms/broadcast` (WebSocket)

Por aca el host manda **su propio audio ya codificado en Opus**, y este servicio
lo publica en su room. Cada mensaje binario es un frame de Opus de 20 ms, que se
reenvia tal cual: ni aca ni en el SFU se transcodifica nada.

Autenticacion por header `Authorization: Bearer <access token de core>` en el
handshake — el cliente nativo (OkHttp) puede mandarlo, un navegador no podria.
No se consulta el permiso de escucha: el host publica su propia actividad, y
quien decide si otro puede oirla es `/rooms/join` del lado del oyente.

**Por que el audio pasa por aca en vez de salir directo del telefono al SFU.**
El SDK de Android solo publica audio a traves de un `AudioDeviceModule`, y el
suyo (`JavaAudioDeviceModule`) **siempre crea un `WebRtcAudioRecord`**: abre el
microfono, sin opcion de desactivarlo. Escribir un ADM propio no alcanza con
Kotlin, porque `getNativeAudioDeviceModulePointer()` tiene que devolver un
puntero a un `webrtc::AudioDeviceModule` de C++. Una app de musica que enciende
el indicador de microfono mientras transmite se lee como que espia al usuario,
asi que el telefono no habla WebRTC en ningun momento: codifica Opus con
`MediaCodec` y lo manda por este socket.

El track se publica con `DisableDTX` y `Stereo` en true. DTX corta la
transmision en los silencios, que en una cancion son parte de la cancion; sin
`Stereo` el track se anuncia mono y se pierde la mitad de la mezcla. Ninguna de
las dos existe como opcion de publicacion en el SDK de Android, lo que es otra
razon por la que publicar desde el servidor sale mejor.

Una sesion por host: si el host reconecta, la anterior se cierra antes de abrir
la nueva. Dos publicadores con la misma identidad en el mismo room se pisan.

**`LIVEKIT_INTERNAL_URL` es obligatoria en el VPS.** El relay entra al room como
un cliente mas, y para eso se conecta al SFU. Si usa la URL publica
(`LIVEKIT_URL`) tiene que salir del VPS, resolver DNS publico y volver a entrar
por el reverse proxy; en la mayoria de las redes eso no funciona y el error es
`could not establish signal connection`. Con `ws://sv-livekit:7880` va por la red
de Docker, directo.

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
| Host excluye a un oyente **mientras escucha** | Solo ese oyente expulsado del SFU; los demas siguen |
| Host apaga el compartir con dos oyentes | Los dos expulsados, el host nunca |
| Se rompe la amistad | Expulsion en **ambos** rooms, motivo `not_friends` |
| signaling apagado al cambiar permisos | El cambio se guarda igual en core; solo no hay expulsion |

## Revocacion en vivo

El permiso se valida al entrar al room. Sin nada mas, quien entro legitimamente
seguiria escuchando aunque despues le saquen el permiso — y "apague el compartir
y me siguieron escuchando" se percibe como una traicion, no como un bug.

### `POST /internal/revoke`

Requiere `X-Internal-Api-Key`. Lo llama **soundvibe-core** cuando cambia algo que
afecta los permisos de un host:

```json
{ "host_id": "<uuid del host>" }
→ { "room": "listen:<uuid>", "checked": 2, "evicted": 1, "removed": ["<uuid>"] }
```

**El diseño clave: core no dice a quien echar.** Este endpoint lista a los
presentes en el room y le vuelve a preguntar a core por cada uno. Asi la politica
vive en un solo lugar y el mismo endpoint sirve para cualquier motivo de
revocacion sin enterarse de cual fue: `share_default` a `nobody`, una excepcion
nueva, o una amistad que se rompe.

Se expulsa con `RemoveParticipant` de la API de administracion del SFU. No se
setea `RevokeTokenTs`: LiveKit por defecto invalida los tokens emitidos antes de
ahora, que es justo lo que hace falta para que el expulsado no vuelva a entrar
con el mismo token que todavia no expiro.

| Status | Que paso |
|---|---|
| 200 | Se reviso el room; `evicted` dice a cuantos se echo |
| 400 | `host_id` invalido |
| 401 | Falta o no coincide la API key |
| 501 | `LIVEKIT_API_URL` sin configurar: la revocacion esta deshabilitada |
| 502 | No se pudo consultar al SFU |

### Como falla

| Situacion | Comportamiento |
|---|---|
| No se puede preguntar a core por un oyente | **No se lo expulsa.** Echar gente por un error de red seria peor que esperar; el permiso se revalida en el proximo join |
| El SFU rechaza una expulsion | Se registra y se sigue con los demas; `evicted` refleja solo los efectivos |
| `LIVEKIT_API_URL` vacia | 501, y core lo registra como problema de despliegue sin reintentar |
| El room esta vacio | 200 con `checked: 0`. Es el caso mas comun |

Notar que aca **no** se falla cerrado, al contrario que en `/rooms/join`. Es
deliberado: en el join, la duda significa no dar acceso; en la revocacion, la
duda significa no cortarle el audio a alguien que probablemente tiene derecho a
estar. El costo de equivocarse es asimetrico en direcciones opuestas.

## Fuera de alcance

Usuarios, amistades y permisos (viven en `soundvibe-core`), cualquier cosa de
Combiyki, y grabacion de sesiones.
