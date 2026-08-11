# Sonda

Un proxy que captura el tráfico de desarrollo local. Apunta un cliente a Sonda
en vez de al servicio con el que habla, y cada request y cada response que lo
cruza queda disponible para buscar — ordenada dentro de la petición a la que
perteneció, reenviable, comparable, y legible tanto por ti como por un agente de
código.

Existe porque depurar entre servicios suele significar leer logs de varios
contenedores, y ninguno contiene el payload. `mitmproxy` resuelve esto muy bien
para HTTP. Para gRPC no hay nada que lo resuelva: `grpcurl` y `grpcui` hacen
llamadas, no observan las que tus servicios se hacen entre sí. Ese es el hueco
al que apunta.

[![CI](https://github.com/NicolasCondezaR/sonda/actions/workflows/ci.yml/badge.svg)](https://github.com/NicolasCondezaR/sonda/actions/workflows/ci.yml)

*[Read me in English](README.md).*

![El campo de eventos: un carril por servicio, los fallos como barras de alto completo](docs/assets/sonda-field.jpg)

> **Estado: fase 19.** Captura, decodificación, almacenamiento, búsqueda, la API
> de consulta, la interfaz web, el replay, el diff estructural, un cliente de
> terminal, la gestión de proyectos, los [árboles de petición](#agentes), el
> [modo stub](#modo-stub), un [servidor MCP para agentes de código](#agentes)
> y [TLS](#tls) funcionan, y todo se levanta con `docker compose up`. La fase 19
> es AMQP 0-9-1: el decodificador del protocolo está escrito y la captura
> todavía no. Ver [Hoja de ruta](#hoja-de-ruta).

## Cómo funciona

Sonda es un proxy explícito: un puerto de escucha por cada servicio observado.
Nada se intercepta a tus espaldas, y un servicio que no está configurado no se
captura.

```
cliente ──▶ sonda :9101 ──▶ tu servicio :3000
                │
                └──▶ SQLite ──▶ API de consulta :9000
```

Dos propiedades sostienen el diseño:

- **El reenvío es byte a byte.** Un depurador que altera el tráfico invalida
  toda conclusión que se saque de él. Los cuerpos de request y response pasan
  intactos, sin importar cuánto de ellos se almacene.
- **Los bytes guardados son el dato.** Los cuerpos se guardan exactamente como
  cruzaron el cable y se decodifican solo al mostrarlos. Re-serializar perdería
  campos desconocidos y reordenaría claves, que es justo lo que vuelve inútil un
  replay — y así una captura se puede volver legible más adelante, cuando
  aparezca su esquema. La única excepción se declara donde aplica: la contraseña
  de [Postgres](#postgresql) se borra de la captura antes de escribirla, porque
  una credencial en un archivo en texto plano ya no se puede sacar después.

## Instalación

Elige la que ya tengas. Nada de esto necesita un toolchain de C ni un SQLite en
el sistema: el driver es Go puro, que es también la razón de que los binarios
sean estáticos y la imagen pese 50 MB.

| | |
|---|---|
| **macOS** | `brew install NicolasCondezaR/tap/sonda` |
| **Windows** | `scoop bucket add nicolascondezar https://github.com/NicolasCondezaR/scoop-bucket`<br>`scoop install sonda` |
| **Go** | `go install github.com/NicolasCondezaR/sonda/cmd/sonda@latest` |
| **Docker** | `docker run -p 127.0.0.1:9000:9000 -v sonda:/data ghcr.io/nicolascondezar/sonda` |
| **Binario** | Descárgalo desde [Releases](https://github.com/NicolasCondezaR/sonda/releases), descomprime y ejecuta |
| **Fuente** | `git clone` y `go build ./cmd/sonda` |

En Linux usa `go install`, la imagen o el tarball: las casks de Homebrew son
solo para macOS.

La línea de Docker publica en `127.0.0.1` y no en todas las interfaces, igual
que el resto de este documento: `sonda.db` guarda las credenciales que hayan
cruzado el cable, en texto plano y sin ningún login delante. Dentro del
contenedor Sonda escucha en `0.0.0.0` —ahí el aislamiento lo pone el
contenedor—, que es lo que hace `-api-listen` en el comando de la imagen; fuera
de un contenedor el valor por omisión sigue siendo loopback.

Los archivos del release traen cuatro binarios, no uno: `sonda`, el cliente de
terminal `sonda-tui`, y los dos servicios de juguete `echo` y `grpcdemo` que usa
el inicio rápido de abajo — para tener algo que capturar sin levantar nada
propio. Los gestores de paquetes instalan solo los dos primeros: un binario
llamado `echo` en el PATH no tiene por qué tapar al del sistema.

```bash
sonda            # el proxy y la interfaz, en http://127.0.0.1:9000
sonda -version   # qué build es este
sonda-tui        # el cliente de terminal
```

Bajar el tarball a mano en macOS tiene un paso extra: Gatekeeper pone en
cuarentena cualquier binario sin firmar que llegue por el navegador. O lo bajas
con `curl`, o limpias la marca una vez. Homebrew lo hace por ti.

```bash
xattr -dr com.apple.quarantine sonda sonda-tui echo grpcdemo
```

## Inicio rápido

### Con Docker

```bash
docker compose up -d
```

Esto levanta Sonda más dos servicios de juguete para tener algo que capturar:
`echo` en HTTP y `grpcdemo` en gRPC. Sus propios puertos no se publican a
propósito: el tráfico debe llegar a través del proxy.

Abre **http://127.0.0.1:9000** y mándale algo:

```bash
curl http://127.0.0.1:9101/ok                 # HTTP, through Sonda
curl 'http://127.0.0.1:9101/fail?status=503'  # a fault, to see one flagged
grpcurl -plaintext -d '{"order_id":"ORD-1"}' \
  127.0.0.1:9201 demo.v1.Orders/GetOrder      # gRPC, through Sonda
```

### Sin Docker

```bash
go build -o sonda ./cmd/sonda
go build -o echo ./examples/echo
go build -o grpcdemo ./examples/grpcdemo

cp sonda.example.yaml sonda.yaml
./echo -addr 127.0.0.1:8081 &
./grpcdemo -addr 127.0.0.1:8082 &
./sonda -config sonda.yaml
```

De aquí en adelante es igual que arriba: abre **http://127.0.0.1:9000** y mándale
las tres peticiones del bloque de Docker. Los puertos son los mismos, porque
`sonda.example.yaml` y la configuración de compose describen los mismos dos
servicios de juguete.

## La interfaz

Sonda se lee como un analizador lógico y no como una tabla de requests, porque
una tabla responde "qué pasó" y nunca "qué estaba pasando al mismo tiempo", que
es la pregunta cuando hablan quince servicios y uno se rompió.

- **Riel de canales.** Una fila por target, con su color tomado del código de
  las puntas de prueba, su total de llamadas y su total de fallos. Los conteos
  no están filtrados: el riel responde "¿está sano este servicio?", así que un
  filtro puesto delante del campo no puede cambiarlo.
- **Campo de eventos.** Un carril por canal contra un eje de tiempo vivo cuyo
  borde derecho es ahora. Una llamada es una marca cuyo ancho es su duración; y
  **un fallo es una forma distinta** —una barra de alto completo— para que
  sobreviva a un canal rojo, a un lector daltónico y a una mirada de reojo.
- **Inspector.** La llamada seleccionada, decodificada, indicando de dónde salió
  el esquema.

Arranca filtrada a fallos, porque ese es el motivo por el que la abriste. `ALL`
cambia al campo completo. Dejar el puntero sobre el campo **congela el trazo**,
para que una marca deje de deslizarse mientras le apuntas; al salir, se reanuda.

`FIND` busca en rutas y en el texto de los payloads, incluidos los que Sonda
solo tiene como bytes. `/` enfoca el buscador y `Escape` cierra el inspector.

La interfaz completa va embebida en el binario: sin Node, sin paso de build, sin
peticiones de red y sin fuentes web.

![Un fallo gRPC: protobuf decodificado por reflection, con el estado real](docs/assets/sonda-grpc-inspector.jpg)

Arriba: una llamada gRPC que devolvió `PermissionDenied`. El estado HTTP es 200
—gRPC reporta el fallo por debajo de HTTP— y la request aparece decodificada con
nombres de campo porque el servicio sirve reflection.

## Proyectos

Un proyecto agrupa los servicios de un sistema —un monorepo, un proyecto
propio, lo que estés tocando hoy— y todo lo suyo se configura desde la interfaz.
Botón **PROJECTS**.

La agrupación no es orden por el orden. Carga las dos cosas que son comunes a
los servicios de un sistema y que si no habría que repetir en cada uno:

- **Un descriptor set para todo el proyecto.** Se sube, no se referencia por
  ruta, así viaja con la base de datos cuando la copias a otra máquina.
- **Una sola respuesta a "¿están abiertos estos puertos?".** Solo el proyecto
  activo escucha, así dos proyectos pueden pedir el mismo puerto sin chocar, y
  cambiar cierra un conjunto y abre el otro sin reiniciar nada.

Las capturas quedan etiquetadas con el proyecto bajo el que se tomaron, así
cambiar no mezcla el tráfico de un sistema con el campo de otro.

### Importar en vez de escribir

Configurar quince servicios a mano es como una herramienta así termina
abandonada después de una tarde. Las direcciones ya están escritas en alguna
parte, así que **IMPORT FROM A FILE** las lee: un `.env` lleno de entradas
`*_URL`, o un archivo de compose con puertos publicados.

Cada entrada vuelve con la línea donde se encontró y con su puerto sugerido ya
probado, así una lectura equivocada o un choque se ven antes de guardar nada. No
se agrega nada hasta que lo digas.

```
+  ms-auth       grpc  http://localhost:50052  127.0.0.1:9152  port already in use
+  ms-billing    grpc  http://localhost:50067  127.0.0.1:9167  line 3: MS_BILLING_GRPC_URL
+  ms-executive  grpc  http://localhost:50064  127.0.0.1:9164  line 2: MS_EXECUTIVE_GRPC_URL
```

Quedan fuera las URLs de base de datos, los brokers de mensajes, las URLs de
callback y cualquier cosa que no sea un servicio al que llamar. Una lista con
una cadena de conexión adentro es peor que una a la que le falte una entrada: la
primera se guarda y se proxea, la segunda se nota.

### El paso que ninguna pantalla elimina

Sonda es un proxy explícito. No ve nada hasta que a quien hace la llamada se
le dice que llame a Sonda — y eso no lo cambia ninguna pantalla de
configuración, porque es el que llama quien decide a dónde van sus requests.

Por eso cada servicio te entrega la línea exacta, lista para copiar:

```
point the caller here:  MS_AUTH_GRPC_URL=127.0.0.1:9152
```

Reinicias al que llama con eso en su entorno y su tráfico aparece en el campo.
No cambia nada en disco, y sacar la variable lo deja como estaba.

El nombre es el que se leyó junto a la dirección al importar el proyecto:
`MS_AUTH_ADDR`, `MS_AUTH_HOST`, lo que diga el archivo. Solo se deriva del
servicio y su protocolo cuando Sonda no tiene registro de ningún nombre — un
servicio agregado a mano, o leído de un compose —, porque un nombre adivinado
entregado junto al real es una línea que cambia una variable que nadie lee.

### El archivo de configuración

`sonda.yaml` sigue cargando los ajustes del proceso: dónde escucha la API,
cuánto cuerpo se guarda, cuánto viven las capturas. Sus `targets` son solo una
**semilla**: se convierten en el primer proyecto la primera vez que se crea una
base de datos, y después se ignoran, así una edición hecha en la interfaz nunca
queda deshecha por un archivo viejo. Arrancar sin archivo de configuración es un
primer uso normal.

## Lo apunté a mi servicio y no veo nada

Este es el primer uso más común de una herramienta así, y todas sus causas se
ven iguales desde afuera: una pantalla vacía. Por eso Sonda responde la pregunta
en vez de dejarla abierta. **Cuando no se capturó nada, el campo deja de estar
vacío y pasa a ser una lectura**: una línea por canal con lo que Sonda sabe de
él, y la misma respuesta está disponible en la terminal, por la API y para un
agente:

```bash
curl -s localhost:9000/api/diagnose | jq
```

```
sonda-tui              el inspector lo muestra mientras el campo está vacío
diagnose_silence       la herramienta MCP que llama un agente cuando falta una captura
```

### Qué puede decirte

Cada canal recibe un veredicto, y los números que lo sostienen están a la vista:

| Veredicto | Qué significa |
|---|---|
| `capturing` | Aquí se están registrando llamadas. Un campo vacío es el filtro, la ventana o el canal seleccionado, no el proxy |
| `listener_down` | El puerto nunca se abrió, casi siempre porque otra cosa lo tiene tomado. Aquí no puede llegar nada, y el error dice qué pasó |
| `connected_not_captured` | Algo llegó a este puerto y nunca se convirtió en una llamada. Sonda vio la conexión y no entendió lo que venía por ella |
| `upstream_unreachable` | El servicio detrás de Sonda rechazó una conexión cuando se le preguntó. Solo se informa después de un sondeo explícito |
| `no_connections` | Nada tocó este puerto desde que se abrió |

La lectura que más trabajo hace es **`connections`**, que cuenta cada conexión
TCP que el puerto aceptó, se haya convertido en llamada o no. Conexiones sin
capturas es un cliente que encontró a Sonda y fue malinterpretado: un cliente
hablando TLS contra un listener en claro o al revés, o un protocolo que Sonda no
proxea. Cero conexiones es un cliente que nunca llegó. Son problemas distintos
con soluciones distintas, y sin ese contador se leen exactamente igual.

Sonda proxea HTTP, gRPC, WebSocket y PostgreSQL. Un cliente de Kafka, de Redis o
de TCP plano apuntado a un puerto de Sonda es aceptado y nunca entendido, y eso
aparece como `connected_not_captured` en vez de como silencio.

### Qué no puede decirte, y lo dice

**Sonda no puede ver un cliente que nunca se conectó a ella.** Un puerto sin
conexiones se lee igual si quien llama sigue hablando directo con el servicio,
si está apuntado a otro puerto, o si simplemente todavía no se ejecutó. No hay
señal honesta que separe esas tres, así que el informe las nombra todas y
entrega lo único que sí las separa: apunta al cliente a Sonda, dispara la
llamada y mira el contador de conexiones. Se mueve incluso cuando la petición
está mal. Si se queda en cero, no está llegando nada a Sonda.

### Sondear un upstream es un efecto secundario

Averiguar si el servicio detrás de Sonda está arriba significa marcarlo, y eso
es tráfico que el usuario no envió. Por eso nunca ocurre solo: ni al cargar la
página, ni al refrescar, ni por un temporizador.

```bash
# solo lee lo que Sonda ya sabe, no toca la red
curl -s localhost:9000/api/diagnose

# además marca una vez cada upstream y corta
curl -s -X POST localhost:9000/api/diagnose
```

Pedirlo es la única forma: `PROBE UPSTREAMS` en el navegador, `p` en la
terminal, `probe_upstreams` en la herramienta MCP. La conexión no envía ningún
byte y va **directo al servicio, nunca por el listener de Sonda**, así que un
sondeo jamás puede aparecer en la lista de capturas como si fuera una llamada
tuya.

### Si sigue sin aparecer nada

- **¿Hay un proyecto activo?** Sin proyecto activo no hay puertos abiertos, y el
  informe lo dice antes que cualquier otra cosa.
- **¿El cliente releyó su configuración?** Un proceso que arrancó antes de
  cambiar la variable de entorno sigue con la dirección vieja.
- **¿El esquema es el correcto?** Un listener que termina TLS no responde nada
  en `http://`, y uno en claro no responde nada en `https://`. Por eso la línea
  que entrega cada servicio lleva el esquema.
- **Revisa el log de la propia Sonda.** Un handshake TLS rechazado se informa
  ahí y en ningún otro lado, porque falla antes de que exista una llamada a la
  que adjuntarlo.

## El cliente de terminal

El mismo instrumento, en una terminal. Es un segundo cliente de la API y no una
segunda implementación: no captura ni guarda nada, lee un Sonda que ya está
corriendo.

```bash
go build -o sonda-tui ./cmd/sonda-tui
./sonda-tui                          # defaults to http://127.0.0.1:9000
./sonda-tui -api http://host:9000

docker compose run --rm tui            # or from the image
```

```
S O N D A  ■ LIVE   FAULTS  ALL    1M  5M  30M                      19 CAPTURED  ·  2 FLAGGED
CHANNEL       CALLS FAULT │-30M         -25M        -20M        -15M        -10M       -5M  NOW
 ■ echo       7     1     │·············│···········│···········│···········│·········█·····
▸■ orders     12    1     │·············│···········│···········│···········│·········█·····
──────────────────────────┴─────────────────────────────────────────────────────────────────
 POST /demo.v1.Orders/Fail
 orders   gRPC   HTTP 200   1.72ms
 gRPC 7 PermissionDenied — no tienes acceso a este pedido
 demo.v1.Orders / Fail   schema from reflection
 REQUEST  1 message(s)
   {
     "code": 7,
     "message": "no tienes acceso a este pedido"
   }
 RESPONSE  0 message(s)
 ↑↓ chan · ←→ call · g/G ends · ⏎ read · esc close · t tree · c contract · r replay · d diff · f faults · w window · h hold · / find · q quit
```

La traducción es casi directa: la monoespaciada es gratis acá, las líneas de un
píxel pasan a caracteres de dibujo de cajas, y los colores de canal se mantienen
iguales. Dos cosas necesitaron otra expresión:

- No hay tamaños de tipografía, así que los cuatro roles pasan a ser peso y
  atenuación.
- Un carril mide una fila, así que un fallo no puede ser una barra más alta. Pasa
  a ser un **bloque completo donde una llamada normal es medio bloque** (`█`
  contra `▄`), con un tercer glifo para una celda que tiene ambos. La forma sigue
  cargando el resultado antes que el color, que es la regla que importa.
- Un servicio **roto a propósito** es un modo y no una llamada, así que el mismo
  bloque queda grabado en el canal, delante de su nombre, y la barra cuenta
  cuántos hay armados: los dos lugares que este cliente tiene para lo que en el
  navegador son la insignia y la lectura de arriba.

| Tecla | |
|---|---|
| `↑` `↓` o `k` `j` | elegir canal |
| `←` `→` o `H` `L` | recorrerlo, llamada por llamada |
| `home` o `g` | saltar a la llamada más antigua del canal |
| `end` o `G` | saltar a la más reciente |
| `enter` | leer la llamada seleccionada |
| `esc` | cerrar lo que esté abierto: inspector, diff, árbol o contrato |
| `t` | ver la petición completa a la que perteneció, como árbol |
| `c` | si este endpoint cambió de forma desde que funcionaba |
| `r` | reenviarla |
| `d` | comparar un reenvío contra su original |
| `f` | solo fallos, o todo |
| `w` | cambiar el barrido |
| `h` | congelar el trazo |
| `/` | buscar; `enter` aplica la búsqueda y `esc` la limpia y sale del campo |
| `q` o `ctrl+c` | salir |

El recorrido avanza llamada por llamada y no celda por celda: una celda vacía no
es algo a lo que apuntar. `h` congela el trazo por la misma razón por la que el
cliente web congela el campo bajo el puntero — una marca que se desliza mientras
le apuntas no se puede seleccionar.

## PowerShell

PowerShell 5.1 reescribe las comillas al pasar argumentos a ejecutables
externos, de modo que `curl.exe` corrompe los cuerpos JSON en silencio:

```powershell
# WRONG — the upstream receives {sku:ABC-9}, quotes stripped
curl.exe -X POST -H "Content-Type: application/json" -d '{"sku":"ABC-9"}' http://127.0.0.1:9101/echo
```

Usa `Invoke-RestMethod`, o pon el cuerpo en un archivo:

```powershell
# Sends a body, and reads what Sonda captured
$body = @{ sku = 'ABC-9'; qty = 3 } | ConvertTo-Json -Compress
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:9101/echo -ContentType 'application/json' -Body $body

(Invoke-RestMethod -Uri http://127.0.0.1:9000/api/calls).calls |
  Select-Object id, method, path, status, duration_ms | Format-Table

$id = (Invoke-RestMethod -Uri 'http://127.0.0.1:9000/api/calls?q=ABC-9').calls[0].id
(Invoke-RestMethod -Uri "http://127.0.0.1:9000/api/calls/$id").request.text
```

```powershell
# curl.exe is fine when the body comes from a file
curl.exe -X POST -H "Content-Type: application/json" --data-binary '@body.json' http://127.0.0.1:9101/echo
```

## gRPC

Pon `protocol: grpc` en un target y apúntalo al puerto del servicio. Sonda le
habla HTTP/2 en claro, reenvía la llamada intacta —trailers incluidos, que es
donde gRPC reporta si la llamada realmente funcionó— y decodifica los mensajes
cuando encuentra un esquema.

```bash
# through Sonda, not straight at the service
grpcurl -plaintext -d '{"order_id":"ORD-777"}' 127.0.0.1:9201 demo.v1.Orders/GetOrder

# only the calls that failed, across HTTP status, gRPC status and transport errors
curl 'http://127.0.0.1:9000/api/calls?failed=true'
```

### De dónde sale el esquema

Tres fuentes, probadas en orden, cada una degradando en la siguiente:

1. **Reflection.** Si el servicio la sirve, Sonda pregunta y no necesita nada
   más. Le pregunta al servicio directamente y no a través del proxy, para que
   su propia contabilidad no termine en tu línea de tiempo.
2. **Un descriptor set en disco.** Para servicios sin reflection, compila los
   protos y apunta al resultado:
   ```bash
   buf build -o descriptors.binpb
   # or: protoc --include_imports --descriptor_set_out=descriptors.binpb path/to/*.proto
   ```
   ```yaml
   - name: orders
     listen: 127.0.0.1:9201
     upstream: http://127.0.0.1:50051
     protocol: grpc
     descriptor_set: ./descriptors.binpb
     reflection: false
   ```
3. **El wire format mismo.** Sin ningún esquema, los mensajes igual vuelven como
   campos numerados con tipos adivinados y la estructura anidada intacta:
   ```json
   [
     {"number": 1, "type": "string",  "value": "ORD-777"},
     {"number": 2, "type": "varint",  "value": 3, "note": "could be an integer, a bool or an enum"},
     {"number": 3, "type": "message", "value": [...], "note": "guessed to be a nested message"}
   ]
   ```
   Las adivinanzas van marcadas como tales. En el cable un varint puede ser un
   int32, un bool o un enum, y decirlo es la diferencia entre una vista útil y
   una engañosa.

`GET /api/schemas` informa qué fuente resolvió cada target, y el motivo cuando
ninguna lo hizo. Es el primer endpoint que hay que mirar cuando faltan nombres
de campo.

Para saber si un servicio sirve reflection:

```bash
grpcurl -plaintext <host>:<port> list
```

### Qué cubre y qué no cubre el soporte de gRPC

- Las llamadas unarias, de server streaming y de client streaming se capturan
  completas, con cada mensaje de ambos lados, no solo el primero.
- El estado sale de los trailers, o de las cabeceras en una respuesta
  *trailers-only*, que es como un servidor reporta un error sin nada que enviar.
  Una llamada gRPC fallida igual lleva HTTP 200; el listado muestra ambos.
- `grpc-message` se decodifica del percent-encoding, así que un error en español
  se lee en español.
- Los mensajes comprimidos se reportan como comprimidos y no se decodifican. La
  codificación se negocia por llamada y adivinarla produciría basura con aire de
  certeza.
- Un target `grpc` igual reenvía el HTTP común que comparte el puerto —endpoints
  de salud y de métricas— y lo clasifica por el content type y no por cómo se
  configuró el target.
- Las llamadas de reflection hechas *a través* del proxy se capturan como
  cualquier otro tráfico. Fíltralas con `?path=demo.v1` o con lo que calce con
  tus propios servicios.

## Replay y diff

Al seleccionar una llamada, el inspector ofrece **REPLAY**. La request sale de
nuevo construida desde los bytes que se guardaron, así que lo que llega al
servicio es lo mismo que llegó la primera vez.

Se envía **a través de Sonda** y no directo al upstream, lo que significa que
el reenvío se captura como cualquier otro tráfico: aparece en el campo, queda
enlazado a la llamada de la que salió, y ambas se pueden comparar de inmediato.

```bash
# replay a call onto the channel it came from
curl -X POST http://127.0.0.1:9000/api/calls/42/replay

# or onto another configured channel, to ask the same request of a second instance
curl -X POST http://127.0.0.1:9000/api/calls/42/replay \
  -H 'Content-Type: application/json' -d '{"target":"orders-staging"}'

curl 'http://127.0.0.1:9000/api/diff?a=42&b=43'
```

El replay solo apunta a un **canal configurado**. No existe un modo de URL
arbitraria: el caso útil es hacerle la misma request a otra instancia que ya
estás observando, y cualquier cosa más amplia convierte un depurador en un
forjador de requests.

**Una captura truncada no se puede reenviar, y Sonda se niega en vez de
intentarlo.** Solo se guardó la cabeza del cuerpo, así que lo que saldría no
sería lo que se capturó, y el resultado llevaría la palabra "replay" siendo una
request distinta. La negativa nombra el arreglo: subir `max_body_bytes` y volver
a capturar.

### El diff es estructural

Los cuerpos se comparan como estructuras parseadas, no como texto. Reordenar
claves o reindentar no son diferencias, así que la respuesta es una lista corta
de rutas en vez de un muro de rojo y verde:

```
~ qty          a 3          b 7
+ nota                      b urgente
```

- El orden de un arreglo **sí** es una diferencia —la posición tiene significado
  en un campo `repeated` de protobuf— mientras que el orden de las claves de un
  objeto no lo es.
- `1290000` y `"1290000"` son el mismo valor. protojson emite int64 como string,
  y reportar eso sería una afirmación sobre codificación, no sobre datos.
- Los streams de gRPC se comparan mensaje por mensaje, así que el mensaje 3 de 5
  puede diferir mientras el resto calza.
- La duración se excluye a propósito: cambia en cada replay y enterraría las
  diferencias que sí significan algo.
- Cuando un lado no es JSON, o no tiene esquema, el diff lo dice e informa si
  los bytes coinciden, en vez de inventar una comparación estructural.

## Qué guarda Sonda, y qué implica eso

Sonda escribe en un archivo SQLite los bytes que cruzaron el cable. **Eso
incluye lo que sea que lleve tu tráfico**: cabeceras `Authorization`, cookies de
sesión, claves de API, datos personales. Nada se redacta al entrar, y es una
decisión deliberada y no un descuido: redactar la captura significaría que ya no
es lo que se envió, lo que rompe tanto la fidelidad sobre la que está construida
la herramienta como el replay junto con ella.

Hay exactamente una excepción, y lo es porque ahí el balance se invierte: la
contraseña de **Postgres** se borra de la captura mientras los bytes pasan. Una
captura de base de datos no se puede reenviar de todos modos, así que no se
pierde nada por no guardarla, y la alternativa es una credencial viva en un
archivo en texto plano. Ver [PostgreSQL](#postgresql).

El otro lugar donde las credenciales sí se retienen es [el servidor
MCP](#agentes), porque ahí las respuestas salen de la máquina.

Lo que se desprende de eso:

- **La base de datos es un archivo plano sin cifrado**, donde sea que apunte
  `database:`. Trátalo como un archivo de logs con credenciales adentro, porque
  eso es.
- **Sonda no tiene autenticación.** Cualquiera que alcance su puerto puede
  leer todas las capturas. Déjalo en `127.0.0.1` —así viene configurado— y no
  publiques el puerto.
- **Es una herramienta de desarrollo local.** Apuntarla a tráfico de producción
  queda fuera de para qué fue construida y fuera de lo que cubre su modelo de
  amenazas.
- La retención acota cuánto viven las capturas, pero es un límite de aseo, no un
  control de seguridad.

## Agentes

Sonda habla el [Model Context Protocol](https://modelcontextprotocol.io), así que
un agente de código puede leer las capturas por su cuenta en vez de que se las
cuenten.

El bucle que reemplaza es el tedioso: el agente escribe código, tú lo corres,
copias un log, se lo pegas, y el agente adivina. Con esto el agente corre el
código y después pregunta qué cruzó de verdad por el cable — decodificado, y sin
pasar por el filtro de lo que alguien decidió loguear.

### Cómo conectarlo

Dos puertas, el mismo servidor detrás.

**Una URL**, si tu cliente acepta una. Nada que instalar, y varios agentes
apuntados a la misma Sonda ven las mismas capturas:

```
http://127.0.0.1:9000/mcp
```

**Un comando**, para clientes que solo hablan por tubería. Reenvía a la Sonda que
ya está corriendo, así que siguen siendo los mismos datos:

```json
{
  "mcpServers": {
    "sonda": { "command": "sonda", "args": ["mcp"] }
  }
}
```

Con `sonda mcp --api http://127.0.0.1:9000` lo apuntas a otra parte.

### Las herramientas

| Herramienta | Qué responde |
|---|---|
| `recent_failures` | "¿Qué se rompió recién?" — casi siempre la primera pregunta |
| `search_calls` | Por servicio, método, ruta, estado o texto en los cuerpos |
| `get_call` | Una llamada completa, decodificada |
| `diff_calls` | "Esta funcionó y esta no, ¿qué cambió?" |
| `trace_call` | Todas las llamadas que fueron parte de la misma petición, como árbol |
| `list_services` | Qué se está observando, en qué puertos, si está escuchando — y qué está respondiendo desde grabaciones o roto a propósito en este momento |
| `schema_status` | De dónde salieron los nombres de campo de cada servicio gRPC: reflection, el descriptor set, o nada |
| `wait_for_call` | Bloquea hasta que aparezca tráfico que calce. Dispara algo y verifica |
| `replay_call` | Reenvía una captura. Marcada como destructiva, el cliente pregunta antes |
| `connect_project` | Configura Sonda para observar un sistema entero, y devuelve la edición que hace pasar el tráfico por ella. Se puede volver a ejecutar |
| `configure_service` | Agrega un servicio, o cambia uno que ya está — el nombre es la identidad, así que llamarla de nuevo mueve el puerto. Una modificación conserva todo lo que no se le pasó |
| `remove_service` | Borra un servicio y dice a qué dirección volver a apuntar a quien llamaba. Pregunta antes |
| `upload_schemas` | Le da al proyecto un descriptor set compilado, para decodificar gRPC donde ningún servicio sirve reflection |
| `activate_project` | Abre los puertos. Pregunta antes |
| `disconnect_project` | Los cierra y devuelve la edición que deshace el apuntado. Pregunta antes |
| `set_stub` | Responder por un servicio desde grabaciones en vez de reenviar. Pregunta antes |
| `break_service` | Agregar latencia, forzar un estado o cortar la conexión. Pregunta antes |
| `contract_drift` | Si esta respuesta cambió de forma desde que funcionaba |
| `trust_certificate` | Dónde vive la autoridad certificadora de Sonda y qué ejecutar para confiar en ella o quitarla |
| `diagnose_silence` | «¿Por qué no veo nada?»: por servicio, si el puerto se abrió, si algo se conectó, qué se capturó y qué causas no se pueden distinguir |

`wait_for_call` es la que convierte a Sonda en un verificador y no solo en un
visor: el agente hace un cambio, dispara la acción y espera lo que debería haber
cruzado. Que no llegue nada también es una respuesta.

### Conectar un proyecto pidiéndolo

> *"Conéctame el monorepo a Sonda."*

El agente lee el `.env` o el compose del proyecto y le pasa el contenido a
`connect_project`. Sonda encuentra los servicios, le asigna un puerto a cada uno,
crea el proyecto — y devuelve la edición exacta a aplicar:

```json
{
  "project": "core-delpagroup",
  "services": 21,
  "active": false,
  "changes": {
    "MS_AUTH_GRPC_URL":  { "from": "localhost:50052", "to": "127.0.0.1:9152" },
    "MS_ADMIN_GRPC_URL": { "from": "localhost:50053", "to": "127.0.0.1:9153" }
  }
}
```

Esa última parte es todo el diseño. **Sonda no puede reapuntar a quien llama** —
es un proxy explícito y no ve nada hasta que a alguien le dicen que le hable. El
agente sí puede: tiene el sistema de archivos y puede reiniciar un proceso. Así
que Sonda sabe el mapeo y el agente tiene las manos.

`disconnect_project` devuelve el inverso. Sin eso, un agente que reapuntó un
`.env` y después paró dejaría el entorno mirando a puertos donde no escucha
nadie.

El inverso solo nombra una variable que Sonda vio de verdad. `MS_AUTH_ADDR`,
`MS_AUTH_HOST` y `MS_AUTH_HTTP_URL` se aceptan igual que `_URL` al entrar, así
que reconstruir el nombre a partir del servicio y su protocolo devolvería
`MS_AUTH_URL`: una variable que nadie lee, mientras la real sigue apuntando a un
puerto que acaba de cerrarse. El nombre que sí vio queda guardado junto al
servicio, así que conectar por la mañana y desconectar por la tarde funciona
aunque Sonda o la máquina se hayan reiniciado en el medio. Cuando el nombre no se
conoce — un servicio agregado a mano, o uno leído de un compose, que nunca tuvo
una variable — vuelve en `restore_by_hand`, con la dirección que hay que buscar y
la dirección que hay que reponer:

```json
{
  "changes": { "MS_AUTH_ADDR": { "from": "127.0.0.1:9152", "to": "localhost:50052" } },
  "restore_by_hand": [
    {
      "service": "web",
      "was_listening_on": "127.0.0.1:9100",
      "point_back_at": "localhost:3000",
      "problem": "Sonda does not know which variable pointed at it…"
    }
  ]
}
```

Crear configuración no molesta a nadie, así que esas herramientas corren
libremente. Abrir y cerrar puertos te puede cambiar la sesión debajo de los pies,
así que esas preguntan antes.

### Preguntar dos veces

Editar el archivo y volver a preguntar es el paso siguiente normal, no un error,
así que `connect_project` acepta el mismo nombre una segunda vez: al proyecto que
ya existe se le agrega, un servicio que ya tiene se actualiza en su lugar con lo
que diga el archivo hoy, y todo lo que el archivo no puede expresar — TLS, si se
verifica el certificado del upstream — se conserva. Una ejecución donde no se
pudo guardar nada borra el proyecto que creó al entrar, así que un intento
fallido no deja nada que limpiar.

`configure_service` funciona igual: una modificación parte del servicio guardado
y cambia solo lo que le pasaste, así que mover un puerto es el proyecto, el
nombre y la dirección nueva. Responde con la dirección a la que hay que apuntar
al llamador, y con la variable donde escribirla cuando Sonda sabe cuál es.

### Qué no está en MCP, a propósito

- **Borrar un proyecto.** `remove_service` cubre el servicio que hay que sacar, y
  volver a conectar el mismo proyecto es como se aplica una configuración que
  cambió, así que nada queda trabado detrás de esa falta. Tirar un proyecto
  entero — sus servicios, sus esquemas, lo que haya adentro — es una decisión con
  una mano humana encima, y el botón está en la interfaz web.
- **El flujo en vivo.** `wait_for_call` responde lo mismo con un límite de
  tiempo, y mantener un stream abierto durante una llamada de herramienta no le
  aporta nada a un agente.
- **Descargar los bytes de la autoridad certificadora.** `trust_certificate`
  devuelve dónde vive y qué ejecutar; instalarla modifica el almacén de confianza
  de la máquina, y ese acto es del usuario, no del agente.
- **Desactivar el filtrado de credenciales.** No existe esa opción en ninguna
  parte, y MCP sería la última superficie en tenerla.

### Las credenciales no salen

Todo lo anterior se filtra antes de salir, con un hueco que se nombra al final
de esta sección. `Authorization`, `Cookie`,
`X-Api-Key`, `password`, `client_secret` y sus distintas grafías vuelven como
`[redacted by Sonda]` — en cabeceras, en cuerpos, y dentro de un JSON anidado en
un cuerpo. **No hay opción para desactivarlo**, a propósito: una bandera para eso
se enciende probando contra un proyecto de juguete y se olvida encendida contra
uno real. La interfaz web sigue mostrando todo, porque ahí el que lee eres tú.

Hay tres pasadas más, que llegan donde comparar un nombre de campo no alcanza:

- **Cadenas de consulta**, en cualquier lugar donde aparezca una URL: la ruta
  capturada, un redirect `Location`, un enlace dentro de un cuerpo.
  `?access_token=`, `?code=` y `?X-Amz-Signature=` se borran y el resto de la
  URL se conserva, porque la ruta es como reconoces la llamada.
- **Postgres**, que es un protocolo orientado a columnas: el nombre sensible y
  el valor sensible llegan en mensajes distintos. Un `RowDescription` se alinea
  contra los `DataRow` que vienen después, y una sentencia que nombra una
  credencial vuelve con su estructura intacta y sus literales borrados —
  incluido el resumen de una línea que el listado muestra antes de que hayas
  pedido nada, y los dos lugares donde una traza repite esa misma línea.
- **La segunda copia de una captura decodificada.** Una sesión de Postgres, un
  WebSocket, un flujo de eventos y una llamada gRPC se sirven dos veces —
  decodificados, y byte a byte tal como cruzaron — y filtrar la primera copia no
  vale nada mientras la segunda está al lado. La copia literal se descarta allí
  donde la vista decodificada la reemplaza. Donde nada la decodifica — la
  petición de un flujo de eventos, una trama gRPC comprimida — se conserva,
  porque descartarla te dejaría sin nada en vez de con menos.

El hueco: un campo protobuf decodificado **sin** esquema tiene un número y no un
nombre, así que no hay nada que comparar y su valor vuelve en claro. Dale al
proyecto un descriptor set, o reflexión al servicio, y el campo recupera su
nombre y se filtra como cualquier otro; `schema_status` dice cuál de los dos
casos estás viendo.

Los cuerpos además vienen acortados por defecto; `get_call` acepta `detail` para
traerlos enteros. `detail` **no** revela credenciales: el filtrado recorre la
respuesta completa primero y el acortado va después, así que la respuesta por
defecto nunca es la más filtrada. Ambas cosas están cubiertas por tests, uno de
ellos a través de una llamada real de herramienta.

`SECURITY.md` enumera hasta dónde llega el filtrado y, más útil todavía, hasta
dónde no.

El endpoint HTTP rechaza las peticiones que traen un `Origin` ajeno, que es lo
que impide que una página abierta en tu propio navegador llegue hasta él por DNS
rebinding y lea tus capturas.

## Sockets y flujos de eventos

Un WebSocket deja de ser peticiones y respuestas en el momento en que su
handshake tiene éxito, así que Sonda sostiene ella misma las dos direcciones en
vez de dejar que el proxy inverso relaye bytes que nadie ve. Lo que se guarda es
el flujo de tramas en crudo por dirección, y las tramas se leen de vuelta al
mirarlas — el mismo arreglo que ya usa el streaming de gRPC.

- El handshake pasa **tal cual**: la clave, los subprotocolos y las extensiones
  negociadas son lo que los dos extremos están acordando, y cambiar cualquiera
  haría que la conversación grabada fuera otra.
- Las tramas del cliente se muestran **desenmascaradas**. La máscara es andamiaje
  del transporte que el receptor quita antes de que nada lea el payload; la clave
  se conserva para que la trama se pueda reproducir exacta.
- Texto, binario, ping, pong, fragmentos y cierre se muestran como lo que son, y
  una trama de cierre reporta su código y su motivo — que suele ser la respuesta
  a por qué el socket dejó de funcionar.
- Un upgrade que el servicio **rechaza** se relaya como la respuesta ordinaria
  que es, para que puedas leer por qué.

**Un socket se captura cuando se cierra**, no mientras está abierto: es un solo
intercambio, y el intercambio no ha terminado. Uno de larga vida está acotado por
el mismo tope de cuerpo que cualquier captura, y dice cuánto no guardó.

Los eventos server-sent no necesitan nada de eso —la respuesta es HTTP
corriente— así que se capturan como siempre y se parten en sus eventos para
mostrarlos, descartando comentarios y keepalives, y marcando como parcial un
último evento cortado.

## GraphQL

Un cliente de GraphQL manda cada operación como el mismo POST a la misma ruta,
así que un servicio que habla GraphQL llega al campo como una sola llamada
repetida cien veces y la línea de tiempo deja de servir para él. Sonda lee la
operación del cuerpo y etiqueta la llamada con ella:
`POST /graphql · mutation Pay`.

- **El documento se detecta por su cuerpo, no por su ruta.** Un POST cuyo JSON
  trae un `query` de tipo texto es una petición GraphQL donde sea que se haya
  enviado: detrás de un prefijo de gateway, en `/api` o en `/graphql`.
- **Un lote son todas las operaciones que lleva.** Los clientes mandan arreglos
  de operaciones en una sola petición; leer solo la primera ocultaría el resto.
- **El inspector muestra la operación**: su tipo y su nombre, los campos de
  primer nivel que pidió, las variables que envió y cada error que trajo la
  respuesta, con su ruta y su `extensions.code`. Los cuerpos en crudo siguen
  ahí al lado.

**Un error de GraphQL es un fallo.** El servidor responde HTTP 200 con un
arreglo `errors`, así que una herramienta que solo mire el código de estado
muestra como éxito justo lo que viniste a buscar. Sonda lo cuenta como fallo en
todos los lugares donde se hace la pregunta: el filtro de fallos, los contadores
del riel de canales, las marcas de fallo del campo, la terminal, el árbol de
trazas y `recent_failures` por MCP. Es el mismo problema que tiene gRPC, y se
resuelve igual.

Sonda lee del documento lo justo para nombrar la operación y nada más. No es un
parser: no valida, no resuelve los fragmentos en sus campos y no conoce tu
esquema. Una respuesta que no es JSON —cortada por el tope de cuerpo, o una
página de error de algo que está delante del servicio— se reporta como
ilegible, no como una llamada sin errores.

## PostgreSQL

Todos los demás protocolos que Sonda captura son un salto entre servicios.
Postgres es el salto de abajo: pone el SQL que una petición ejecutó de verdad en
el campo, debajo de la llamada HTTP que lo provocó, una fila por sentencia, y un
N+1 deja de ser una sospecha para volverse cuarenta filas bajo un solo handler.

Una base de datos se declara como cualquier otro servicio, con
`protocol: postgres` y un upstream que nombra el transporte:

```yaml
  - name: orders-db
    listen: 127.0.0.1:9301
    upstream: postgres://127.0.0.1:5432
    protocol: postgres
```

Después se apunta el DSN de la aplicación a la dirección de escucha,
conservando su propio usuario, su contraseña y su base de datos:

```
DATABASE_URL=postgres://app:secret@127.0.0.1:9301/orders
```

**El upstream no lleva credenciales, y Sonda rechaza uno que las traiga.**
Reenvía el handshake del propio cliente sin tocarlo, así que no tiene ningún uso
para un usuario ni para una contraseña, y una contraseña en la configuración
sería una contraseña escrita dentro de `sonda.db`.

### La contraseña nunca llega a la captura

Este es el único lugar donde Sonda reescribe lo que guarda, y la razón es que la
alternativa ya no se puede corregir después. El intercambio de inicio lleva la
contraseña. Si los bytes se guardaran tal como llegaron, el secreto quedaría en
`sonda.db` en texto plano y podría llegar a un agente por MCP, cuya redacción lee
nombres de campo, cadenas de consulta y la forma de un intercambio Postgres, y un
handshake de inicio no es nada de eso: la contraseña es una corrida de bytes con
prefijo de longitud dentro de un flujo TCP, sin nombre alguno.

Por eso los bytes de la credencial se borran en la derivación, al pasar, antes
de que se guarde nada: el cuerpo del PasswordMessage y de las respuestas SASL,
los desafíos de autenticación del servidor y la clave de cancelación en ambas
direcciones. Se reemplazan por relleno del mismo largo, de modo que cada campo
de longitud del flujo sigue siendo cierto y la captura se sigue leyendo como una
conversación. Lo que sobrevive es que hubo una autenticación y con qué mecanismo
—`sasl`, `md5_password`, `cleartext_password`—, que es la parte a la que quien
lee tiene derecho.

**Lo que se reenvía queda intacto.** La reescritura se aplica a la copia que
Sonda guarda, nunca a los bytes que recibe la base de datos: la contraseña real
llega al servidor y el login funciona. Las dos mitades están cubiertas por
pruebas.

### Qué es una captura

- **Una captura es una sentencia, no una conexión.** El protocolo regala el
  límite: una consulta simple es `Q` → resultados → `ReadyForQuery`, una
  extendida es Parse/Bind/Describe/Execute/Sync → resultados → `ReadyForQuery`,
  y la `Z` cierra el ciclo en las dos. Cada fila lleva el SQL, los valores que
  se le asociaron, lo que respondió el servidor —la etiqueta de comando, el
  número de filas o el error— y el tiempo de esa sentencia sola.
- **El SQL queda colgado bajo la petición que lo ejecutó**, y no hace falta
  ningún mecanismo nuevo para correlacionarlos. Sonda ya arma el árbol por
  contención: una llamada que empieza no antes y termina no después que otra es
  hija suya, y una sentencia ejecutada durante una petición HTTP está contenida
  en ella por definición. Eso solo funciona porque la captura se mide desde la
  sentencia y no desde la conexión, que es justamente la razón de partirla:
  detrás de un pool, una conexión lleva horas de SQL de la aplicación, y horas
  de SQL no se pueden colgar de ninguna petición.
- **La apertura de la conexión viaja en su primera sentencia.** Los parámetros
  de inicio, el mecanismo que se exigió y los ajustes del servidor ocurren una
  sola vez, y son contexto de lo primero que la conexión ejecutó, no una fila
  propia: una fila sin SQL adentro sería ruido en cada conexión de un pool. Pero
  callar que hubo una autenticación no es algo que un depurador pueda hacer, así
  que una conexión que se autenticó y no ejecutó nada sí recibe su propia fila,
  con el método `SESSION`.
- **La ruta es la base de datos**, leída del mensaje de inicio en vez de
  inventada, y puesta en cada sentencia de la conexión: solo la primera contiene
  el mensaje del que salió. El método es `STATEMENT`. No hay estado HTTP, y no
  se muestra ninguno.
- **El listado lleva la sentencia misma** y cómo terminó
  (`SELECT id FROM orders WHERE total > 100 -> SELECT 12`), o el error si lo
  hubo. Se nombran todos los resultados del ciclo, no solo el primero: un `COPY`
  y una consulta simple con varias sentencias responden una vez con varios.
- **Una sentencia que nunca terminó igual queda registrada, y lo dice.** Se cayó
  la conexión a mitad de la consulta, o la captura terminó con la sentencia en
  vuelo: la fila se escribe con lo que cruzó y con un error que dice que nunca
  llegó un `ReadyForQuery`. Darla por exitosa sería mentir, y descartarla sería
  perder justamente la sentencia que valía la pena.
- **La sentencia se busca completa** —el SQL, los valores asociados, las
  etiquetas de comando y la queja del servidor—, no solo la línea de resumen.
- **El inspector muestra los mensajes**: las sentencias, sus parámetros con
  `NULL` distinguido de la cadena vacía, las columnas descritas, las etiquetas
  de comando, el estado de la transacción y cualquier error del servidor con su
  SQLSTATE, su detalle y su sugerencia. Las filas de datos se cuentan en vez de
  dibujarse una por una.

**Un error de SQL es un fallo.** Llega como un `ErrorResponse` dentro del flujo,
sin ningún código de estado en ninguna parte, así que una herramienta que solo
mire el transporte lo mostraría como una sentencia sana. Sonda lo cuenta como
fallo en todos los lugares donde se hace la pregunta: el filtro de fallos, el
riel de canales, las marcas de fallo del campo, la terminal, el árbol de trazas
y `recent_failures` por MCP. Es el mismo problema que tienen gRPC y GraphQL, y
se resuelve igual.

**Una sentencia no se puede reenviar.** Pertenece a una conexión, una sesión y
una transacción que ya no existen, y mandar el SQL de nuevo lo ejecutaría en
otro lugar. Todas las superficies se niegan, incluida la propia API, así que un
agente recibe la misma respuesta que el navegador.

## TLS

Bajo las mismas tres letras hay dos problemas distintos, y Sonda los resuelve
por separado.

### El upstream habla TLS

Se declara con el esquema que tiene y nada más cambia:

```yaml
- name: payments
  listen: 127.0.0.1:9103
  upstream: https://api.payments.example.com
  protocol: http
```

El certificado se verifica igual que lo verificaría cualquier otro cliente. Un
target gRPC detrás de TLS funciona igual —negocia HTTP/2 por ALPN en vez de
h2c— y un upstream escrito sin puerto recibe el que implica su esquema.

Un upstream cuyo certificado no verifica responde 502 con el motivo, y la
captura registra el error de transporte. Es deliberado: un proxy que aceptara
cualquier certificado en silencio dejaría sin valor cada lectura de "verificado"
de la herramienta.

### El cliente habla TLS

Hay clientes que se niegan a usar `http://`. Con `tls: true` Sonda responde ese
puerto con un certificado propio:

```yaml
- name: web-api
  listen: 127.0.0.1:9104
  upstream: http://127.0.0.1:3000
  protocol: http
  tls: true
```

Entonces se apunta al llamador a `https://127.0.0.1:9104`, y la interfaz entrega
esa línea exacta. El certificado se emite al vuelo para el nombre que pidió el
cliente —el nombre SNI, o la dirección a la que se conectó cuando no envió
ninguno—, se guarda en memoria por nombre y lo firma una autoridad que Sonda
genera la primera vez que hace falta.

Postgres es la excepción. Una base de datos negocia el cifrado dentro de su
propio protocolo y no antes, así que un listener TLS delante de ella estaría
respondiendo un handshake que ningún cliente envía. Sonda rechaza la opción en
ese caso en vez de aceptarla e ignorarla.

### Confiar en la autoridad certificadora

**Sonda no instala nada.** Escribe dos archivos junto a la base de datos
—`sonda-ca.pem` y `sonda-ca-key.pem`, la clave legible solo por su dueño—,
imprime qué hay que ejecutar y se detiene ahí. Modificar el almacén de
confianza de la máquina es una decisión que le corresponde tomar a quien la usa,
y una herramienta de depuración que lo hiciera en silencio sería
indistinguible de un programa malicioso.

Los comandos se imprimen la primera vez que la autoridad hace falta de verdad
—cuando arranca por primera vez un servicio con `tls: true`, que no es lo mismo
que la primera ejecución: un Sonda sin ningún objetivo TLS nunca crea una
autoridad y no imprime nada—. También aparecen en el panel de autoridad
certificadora de la interfaz y los devuelve la herramienta MCP
`trust_certificate`. La opción acotada suele ser la correcta, porque no confía
en nada más de la máquina y no deja nada que deshacer:

```bash
curl --cacert ./sonda-ca.pem https://127.0.0.1:9104/
NODE_EXTRA_CA_CERTS=./sonda-ca.pem npm start
SSL_CERT_FILE=./sonda-ca.pem go run ./cmd/whatever
REQUESTS_CA_BUNDLE=./sonda-ca.pem python app.py
```

Para toda la máquina —que es lo que necesita un navegador— hay que ejecutar uno
mismo la línea de su plataforma:

```bash
# macOS
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ./sonda-ca.pem
# Windows
certutil -user -addstore Root .\sonda-ca.pem
# Linux (Debian, Ubuntu)
sudo cp ./sonda-ca.pem /usr/local/share/ca-certificates/sonda-ca.crt && sudo update-ca-certificates
# Linux (Fedora, RHEL)
sudo cp ./sonda-ca.pem /etc/pki/ca-trust/source/anchors/sonda-ca.pem && sudo update-ca-trust
```

Firefox mantiene su propio almacén: **Configuración → Privacidad y seguridad →
Certificados → Ver certificados → Autoridades → Importar**, y marcar *Confiar en
esta CA para identificar sitios web*.

Y para quitarla de nuevo —primero se retira la confianza y después se borran los
archivos, o la máquina queda confiando en una raíz de la que nadie puede dar
cuenta—:

```bash
# macOS
sudo security delete-certificate -c "Sonda local CA (tu-hostname)" /Library/Keychains/System.keychain
# Windows — el número de serie se imprime con el certificado y se ve en la interfaz
certutil -user -delstore Root <serie>
# Linux (Debian, Ubuntu)
sudo rm /usr/local/share/ca-certificates/sonda-ca.crt && sudo update-ca-certificates --fresh
# Linux (Fedora, RHEL)
sudo rm /etc/pki/ca-trust/source/anchors/sonda-ca.pem && sudo update-ca-trust

rm ./sonda-ca.pem ./sonda-ca-key.pem
```

La autoridad se identifica a sí misma y nombra la máquina donde se creó —`Sonda
local CA (hostname)`—, así que se encuentra en un almacén de confianza un año
después, y caduca al año, de modo que una que quedó olvidada deja de importar
sola. La clave privada nunca se escribe en los logs, nunca la devuelve la API,
nunca es accesible por MCP y nunca queda dentro de una captura; `SECURITY.md`
explica qué significa eso al copiar la base de datos de una máquina a otra.

### No verificar un upstream

El caso real para el que esto existe es un servicio con certificado
autofirmado. Hay exactamente una forma de saltarse la verificación, y es por
servicio:

```yaml
- name: staging
  listen: 127.0.0.1:9105
  upstream: https://staging.internal:8443
  protocol: http
  insecure_skip_verify: true
```

No hay un interruptor global y no va a haberlo: "confío en este contenedor" y
"confío en cualquier cosa" no son la misma afirmación. Sonda rechaza la opción
en un upstream en claro, donde no significaría nada mientras el servicio
seguiría apareciendo como no verificado en todas partes.

Y nunca es silenciosa. El servicio queda marcado en la interfaz web, en el riel
de canales de la terminal y en `list_services`, y cada captura tomada a través
de él lleva `upstream_insecure`, así que una respuesta leída meses después sigue
diciendo si alguien llegó a comprobar quién la envió.

## Deriva de contratos

En un monorepo donde nadie versiona un contrato, un campo que se fue callado o
que cambió de tipo rompe al que llama días después, lejos del cambio que lo
causó. Sonda ya tiene todas las respuestas que un servicio dio alguna vez.

```
CONTRACT                                vs capture #412
−  data.items[].currency                    was string
~  data.total                          number -> string
+  data.meta.cached                              boolean

2 of these would break a caller.
```

Compara **formas, no valores**. Dos llamadas que devuelven precios distintos no
son deriva; una que devuelve el precio como número y otra como texto, sí.

- La referencia es la **captura más vieja que Sonda tenga** del mismo endpoint,
  no un esquema que alguien deba mantener — una referencia que nadie actualiza
  es una referencia que en tres semanas ya no está.
- Una lista se colapsa a la forma de sus elementos. Doscientos pedidos reportan
  la forma de un pedido, o el campo que cambió quedaría enterrado bajo sí mismo.
- Un **campo nullable no es deriva.** Marcarlos todos entierra los cambios que
  importan bajo ruido sobre el que nadie puede actuar.
- Una lista vacía no afirma nada sobre lo que contiene.
- **Agregar un campo es seguro**; perder uno o cambiarle el tipo es lo que tumba
  a quien llama, y el reporte dice cuál es cuál.

En la interfaz es una sección del inspector, en el terminal es `c`, y para un
agente es `contract_drift`. Es lo único en Sonda que no toca el proxy: solo lee
lo que ya estaba guardado.

## Romper a propósito

La lógica de reintentos, los timeouts y la degradación se escriben una vez y
después no se ejercitan nunca, porque hacer fallar un servicio de verdad es lo
bastante engorroso como para que nadie lo haga. Sonda ya está en el camino.

```bash
curl -X POST http://127.0.0.1:9000/api/faults   -d '{"service":"ms-rates","latency_ms":2000}'
curl -X POST http://127.0.0.1:9000/api/faults   -d '{"service":"ms-seo","status":503,"one_in":3}'
curl -X POST http://127.0.0.1:9000/api/faults   -d '{"service":"ms-auth","cut":true}'
```

Desde un agente, `break_service` hace lo mismo, y pregunta antes.

En la interfaz está sobre el servicio mismo, dentro de **PROJECTS**: la fila
lleva `LATENCY MS`, `HTTP STATUS`, `CUT` y `ONE CALL IN` junto a una tecla
`ARM`, y queda leyendo **BROKEN ON PURPOSE** con la regla en vigor hasta que
`RESTORE` la saca. Una regla que no haría nada — sin latencia, sin estado y sin
corte — se rechaza, y lo que el panel muestra es ese rechazo y no una regla que
nunca se armó.

El terminal lee ese mismo estado y no lo cambia, que es el nivel al que ya
trabaja con el stub: la barra cuenta cuántas reglas hay en vigor, el canal lleva
el bloque de fallo delante de su nombre, y el inspector nombra la regla junto a
la llamada que se está leyendo.

**La latencia deja pasar la llamada** — el servicio igual responde, solo que
tarda más, que es el caso que un timeout debe atrapar. **Un estado o un corte
terminan la llamada en Sonda**: el servicio nunca se alcanza.

### Determinista, no aleatorio

`one_in: 3` significa una llamada de cada tres, en ese orden, en cada corrida.
Un porcentaje se comportaría distinto cada vez y convertiría un test que falla
en una moneda al aire; una secuencia que puedes reproducir es la única que sirve
para depurar. Cambiar una regla reinicia su cuenta.

### Nunca puede pasar por un fallo real

Todo fallo inyectado lleva **`X-Sonda-Fault`** con el motivo, se guarda marcado
como inyectado, y aparece señalado en el campo, en el inspector y en el
terminal.

Una regla en vigor se dice donde se están leyendo los fallos, y no solo donde se
armó: el canal muestra **BROKEN**, y tanto la lectura de arriba en el navegador
como la barra del terminal cuentan cuántas hay. Eso pesa sobre todo con
`one_in` mayor que 1, donde la mayoría de las llamadas pasa intacta y las
inyectadas, por sí solas, parecen un servicio simplemente inestable.

Las reglas se olvidan al reiniciar Sonda, por lo mismo que el stub: un servicio
que falla desde el martes por una regla que nadie recuerda haber puesto es una
peor tarde que el bug que se estaba persiguiendo. Nada de esto se escribe en la
base de datos.

## Modo stub

Sonda ya tiene los bytes exactos de cada respuesta que un servicio dio alguna vez.
Devolverlos en vez de reenviar convierte la misma herramienta en otra cosa:

- Trabajar en el front con su backend apagado
- Correr un test sin levantar veintiún procesos
- Reproducir un bug desde una captura, en un portátil, sin el entorno que lo produjo

```bash
curl -X POST http://127.0.0.1:9000/api/stub   -d '{"service":"ms-rates","enabled":true}'
```

Desde un agente, `set_stub` hace lo mismo. Pregunta antes: un servicio
respondiendo calladamente desde la semana pasada es justo la sorpresa que
conviene confirmar.

### Por qué no se puede confundir con lo real

Una respuesta grabada que pasa por viva es el fallo que esta función tiene que
evitar, así que cuatro cosas lo hacen difícil por accidente:

- Toda respuesta desde stub lleva **`X-Sonda-Stub: <id de la captura>`**
- El intercambio se captura igual, enlazado a la grabación de la que salió, así
  el campo nunca muestra como ocurrido un tráfico que no ocurrió
- **El stub se olvida al reiniciar Sonda.** Nunca se escribe en la base: un stub
  que sobrevive a un reinicio es uno que nadie recuerda haber encendido
- Una petición sin grabación recibe un **501 que se explica**, no una respuesta
  inventada ni un 200 vacío en silencio

### Qué grabación responde

Un cuerpo de request idéntico gana de plano — esa es la diferencia entre
reproducir *la respuesta a GetOrder* y *la respuesta a GetOrder(ORD-1)*, y un
test al que le entregan el pedido de otro está peor que uno al que le entregan un
error. Si no hay ninguno, la llamada más reciente al mismo método y ruta.

Las capturas que fueron ellas mismas un stub nunca se reutilizan. Sin eso, dejar
el stub encendido iría alimentando a Sonda con sus propias respuestas.

gRPC también funciona: los trailers grabados se reproducen, así el cliente recibe
el `grpc-status` real en vez de esperar uno que no llega.

## Configuración

Copia `sonda.example.yaml` a `sonda.yaml` y agrega una entrada por servicio.
Una clave desconocida es un error de arranque y no un valor por defecto
silencioso, así que una errata no se convierte en una hora de confusión.

```yaml
api_listen: 127.0.0.1:9000
database: sonda.db
max_body_bytes: 262144   # kept per body; the full body always reaches its destination
buffer_size: 1024        # captures buffered in memory before they are written

retention:
  max_calls: 50000
  max_age: 24h
  interval: 1m

targets:
  - name: admin-api
    listen: 127.0.0.1:9102
    upstream: http://127.0.0.1:3000
    protocol: http     # http, grpc o postgres

  - name: payments
    listen: 127.0.0.1:9103
    upstream: https://api.payments.example.com  # verificado como lo haría cualquier cliente
    protocol: http
    tls: true                    # responder este puerto con certificado, para clientes que rechazan http://
    insecure_skip_verify: false  # por servicio, nunca global. Ver la sección TLS
```

Después apunta a `127.0.0.1:9102` lo que sea que llame a `admin-api`. El mismo
binario y el mismo archivo sirven para servicios en contenedores y para
servicios corriendo de forma nativa, que es justamente el punto: un stack local
de verdad suele ser las dos cosas.

Dentro de Docker, usa `host.docker.internal` para alcanzar un servicio que corre
en el host. Ver `sonda.docker.yaml`.

## API

| Método y ruta | Para qué |
|---|---|
| `GET /api/calls` | Lista las capturas, más recientes primero. Filtros: `target`, `method`, `path`, `status`, `protocol`, `grpc_status`, `failed`, `q`, `since`, `until`, `limit`, `before_id`. |
| `GET /api/calls/{id}` | Una captura con cabeceras y cuerpos. |
| `GET /api/targets` | Los targets configurados. |
| `GET /api/schemas` | Por cada target gRPC: qué fuente de esquema resolvió, o por qué ninguna. |
| `POST /api/calls/{id}/replay` | Reenvía la llamada, opcionalmente a otro canal. |
| `GET /api/diff?a=&b=` | Comparación estructural de dos llamadas. |
| `GET /api/trace?call=` | La petición completa a la que perteneció una llamada, como árbol. |
| `GET /api/stub` | Qué servicios están respondiendo desde grabaciones. |
| `POST /api/stub` | Activar o desactivar el stub de un servicio, o limpiarlo todo. |
| `GET /api/faults` | Qué servicios se están rompiendo a propósito, y cómo. |
| `GET /api/drift?target=` | Si un endpoint sigue respondiendo la forma que respondía. |
| `POST /api/faults` | Poner o quitar una regla de fallo. |
| `GET /api/projects` | Los proyectos, sus servicios y qué está escuchando de verdad. |
| `POST /api/projects` | Crear uno. `PATCH`/`DELETE /api/projects/{id}` renombran y borran. |
| `POST /api/projects/{id}/activate` | Cierra los puertos del proyecto actual y abre los de este. |
| `POST /api/projects/deactivate` | Cierra todos los puertos. No borra nada, y activar lo devuelve todo. |
| `POST /api/projects/{id}/descriptor` | Sube los esquemas compilados de todo el proyecto. |
| `POST /api/projects/{id}/services` | Agrega o actualiza un servicio. `DELETE /api/services/{id}` quita uno. |
| `POST /api/discover` | Lee servicios de un `.env` o un compose sin guardar nada. |
| `GET /api/runtime` | Qué proyecto está activo y qué está escuchando de verdad, incluyendo cuántas conexiones aceptó cada puerto. |
| `GET /api/diagnose` | Por qué no se está capturando nada, servicio por servicio. Solo lee lo que Sonda ya sabe y no toca la red. |
| `POST /api/diagnose` | El mismo informe, más una conexión TCP a cada upstream. Es un efecto secundario, y por eso no está en el `GET`. |
| `GET /api/tls` | La autoridad certificadora y los comandos exactos para confiar en ella y para quitarla. Nunca la clave privada. |
| `GET /api/tls/ca.pem` | Descarga el certificado de la CA. Útil cuando Sonda corre en un contenedor. |
| `GET /api/stats` | Cantidad de capturas, rango de tiempo y llamadas descartadas bajo carga. |
| `GET /api/stream` | Eventos server-sent: cada captura en el momento en que se guarda. Es lo que lee el campo en vivo. |
| `GET /health` | Liveness. |

El listado no lleva cuerpos a propósito: unos cientos de llamadas con payloads
adjuntos es inusable. Los cuerpos vienen del endpoint de detalle, como `text`
cuando el contenido es UTF-8 válido y como `base64` cuando no lo es. La API
nunca adivina: informa qué son los bytes.

`q` busca en rutas y en payloads de texto. Se trata como una frase literal, así
que `"sku":"ABC-9"` y `/v1/orders` funcionan tal como se escriben en vez de
leerse como operadores de consulta.

## Comportamiento que conviene conocer

- **La captura nunca retrasa el tráfico.** Las escrituras ocurren en una
  goroutine aparte detrás de un búfer acotado. Ante una ráfaga, se descartan
  capturas antes que frenar el sistema que estás intentando observar — y
  `GET /api/stats` informa cuántas se perdieron, para que la pérdida sea visible
  en vez de silenciosa.
- **Los cuerpos grandes se truncan solo en el almacén.** Una request de 500 KB
  con un tope de 256 KB se reenvía completa y se guarda como 256 KB, marcada
  como `truncated`, con `size` informando los 500 KB reales.
- **El texto mal codificado igual se puede buscar.** Un acento en latin-1 de un
  servicio que nunca arregló su charset no vuelve inencontrable la llamada; el
  índice se sanea mientras los bytes guardados quedan exactos. Los payloads
  realmente binarios no se indexan.
- **Un upstream inalcanzable también se captura.** Sonda responde 502 y
  registra el error de transporte, así que el fallo está en la línea de tiempo
  en vez de faltar. Un 502 que vino del upstream mismo tiene el campo `error`
  vacío.
- **La retención corre por temporizador**, aplicando primero la antigüedad y
  después el tope de filas.
- **Ctrl+C drena el búfer** antes de salir.

## Cuánto cuesta

Sonda parte de la idea de que un depurador que altera el tráfico invalida toda
conclusión sacada de él, y el tiempo es parte de lo que un proxy altera. Si
estás persiguiendo un timeout en el límite entre dos servicios, tienes derecho a
saber cuánto agregó el instrumento que quedó en el medio. Por eso el proxy se
mide contra sí mismo, en `internal/proxy/bench_test.go`.

| HTTP, cuerpo de 256 bytes | µs por llamada |
|---|---|
| Directo al upstream, sin proxy | ~157 |
| Por un `httputil.ReverseProxy` estándar, sin Sonda | ~430 |
| Por Sonda, capturando | ~540 |
| Por Sonda, con el recorder reemplazado por uno que descarta | ~452 |

| gRPC unario | µs por llamada |
|---|---|
| Directo al servicio, sin proxy | ~252 |
| Por un reverse proxy estándar, sin Sonda | ~797 |
| Por Sonda, capturando | ~960 |

Lo que dicen esas filas:

- **La mayor parte de la latencia agregada es el precio de haber puesto un
  proxy, no el de capturar.** En HTTP pequeño, el segundo salto TCP más el
  propio `ReverseProxy` cuestan ~273 µs, y lo que Sonda suma encima son ~110 µs.
  En gRPC la forma es la misma: ~545 µs del proxy pelado y ~163 µs que agrega
  Sonda. Quien ponga cualquier proxy en el camino paga la primera parte; solo la
  segunda es de Sonda.
- **La mayor parte de lo que suma Sonda es el recorder** — unos 88 µs de los
  110. Eso no es la petición esperando una escritura en la base: `Record` es un
  envío no bloqueante a un canal con búfer, y descarta antes que bloquear. Es la
  captura que se arma y sus cuerpos que se copian en la goroutine de la petición,
  más la goroutine que drena escribiendo SQLite en las mismas CPU. Es un costo
  real del diseño y conviene decirlo en vez de esconderlo.
- **El streaming se mide como retraso por mensaje**, no como throughput, porque
  el tiempo total de un flujo lo domina lo que el servidor espere entre mensajes.
  Directo son ~1,63 ms y por Sonda ~2,20 ms, así que un mensaje llega alrededor
  de 0,6 ms más tarde. Las cifras absolutas no son tiempo de tránsito: incluyen
  la espera del servidor de prueba pasándose de largo, que la aritmética le carga
  al tránsito. Ambos casos corren contra el mismo servidor, así que lo único que
  significa algo es la diferencia.
- **Capturar un cuerpo grande reserva una copia de él.** Una request de 1 MiB y
  una response de 1 MiB, con `max_body_bytes` por encima del cuerpo, reservan
  ~7,4 MB por llamada; la misma llamada con el cuerpo pasado del tope reserva
  ~2,3 MB. Ese costo aparece como presión de memoria y no como latencia, y
  `max_body_bytes` es la perilla que lo controla.

Una décima de milisegundo por encima de lo que ya cuesta cualquier proxy es un
número chico para una herramienta que corre en la misma máquina que los
servicios que observa. Si es lo bastante chico o no es un juicio sobre lo que
estás depurando.

**Estas cifras son una medición, no una especificación.** Salen de un solo
portátil — un Intel Core i9-14900HX sobre Windows — por loopback, contra
servidores `httptest`, con la máquina haciendo lo que estuviera haciendo, a
`-benchtime=2000x -count=5` y tomando la corrida del medio de las cinco. Nada de
esto se promete: otra máquina, una red real o una más cargada dan otros números.
Si el costo influye en una decisión que tienes que tomar, córrelos tú mismo:

```bash
go test ./internal/proxy/ -bench=. -benchmem -run=XXX
```

`CONTRIBUTING.md` explica cómo leer la salida, que es menos evidente de lo que
parece.

## Hoja de ruta

| Fase | Alcance | Estado |
|---|---|---|
| 1 | Captura HTTP, almacenamiento, búsqueda, API de consulta | listo |
| 2 | gRPC: h2c, trailers, desenmarcado, decodificación de protobuf | listo |
| 3 | Interfaz web con línea de tiempo en vivo | listo |
| 4 | Replay y diff estructural | listo |
| 5 | Empaquetado y documentación | listo |
| 6 | TUI, como segundo cliente de la misma API | listo |
| 7 | Proyectos: servicios agrupados, configurados desde la interfaz, importados de un archivo | listo |
| 8 | Servidor MCP, para que un agente de código lea las capturas por su cuenta | listo |
| 9 | Configuración por MCP: conectar un proyecto entero pidiéndolo | listo |
| 10 | Correlación: las llamadas de una petición, ordenadas como árbol | listo |
| 11 | Modo stub: responder desde una grabación en vez de reenviar | listo |
| 12 | El árbol y el stub, en todas las superficies: web, terminal y MCP | listo |
| 13 | WebSocket y eventos server-sent | listo |
| 14 | Inyección de fallos: latencia, estados forzados, conexiones cortadas, armadas y leídas en todas las superficies | listo |
| 15 | Deriva de contratos: un campo que se fue, uno que cambió de tipo | listo |
| 16 | GraphQL: la operación detrás de cada POST idéntico, y sus errores contados como fallos | listo |
| 17 | PostgreSQL: una captura por sentencia, colgada bajo la petición que la ejecutó, con las credenciales borradas antes de guardarlas | listo |
| 18 | TLS: terminado para el cliente desde una autoridad local que Sonda nunca instala, hablado hacia el upstream, y registrado en cada captura como verificado o no | listo |
| 19 | AMQP 0-9-1: el decodificador del protocolo, con las credenciales nombradas pero no decodificadas | decodificador listo, captura sin construir |

Kafka falta de esa tabla a propósito. Por qué, va abajo.

### Por qué falta Kafka

**Hoy Sonda no tiene ningún listener de Kafka.** `protocol:` acepta `http`,
`grpc` y `postgres`, y las tramas de Kafka que lleguen a un listener HTTP se
leen como una petición malformada. Nada de esta sección se puede usar todavía:
es la explicación de por qué falta la fila, no una receta.

Poner un proxy delante de un broker no funciona, y la razón no es el protocolo.
Un cliente de Kafka usa su primera conexión solo para preguntar dónde están los
brokers. La respuesta es lo que el broker publique como `advertised.listeners`,
y el cliente abre entonces **conexiones nuevas directamente a esas direcciones**
y manda por ahí todo lo que importa: las publicaciones, las lecturas y cada
llamada de grupo. Un proxy en medio ve el saludo inicial y después nada, para
siempre, mientras el tráfico por el que alguien conectó un depurador pasa por
al lado.

Conseguir que el cliente se quede significaría reescribir al vuelo las
direcciones de los brokers dentro de esas respuestas. Sonda no lo va a hacer,
por la razón que encabeza `internal/proxy/proxy.go`: el reenvío es byte a byte,
y una captura de un clúster que no existe es peor que no capturar nada.
Reescribir direcciones es para lo que está una pasarela de Kafka, que es
justamente lo contrario de esto.

Hay una salida que no reescribe nada: apuntar el broker a Sonda en vez de poner
a Sonda delante del broker, de modo que toda dirección que reciba el cliente sea
una a la que se pretendía que se conectara:

```yaml
# el broker escucharía en 9192 y Sonda se quedaría con 9092
KAFKA_ADVERTISED_LISTENERS: PLAINTEXT://localhost:9092
```

Así que si alguna vez se construye la captura de Kafka, así va a estar cableada,
y lo que falta por escribir es un protocolo `kafka` con su listener crudo y un
decodificador al lado de `pgwire` y `amqpwire`. La parte difícil nunca fue el
protocolo.

### Limitaciones

- Sonda es un proxy explícito, no un interceptor. Lee un intercambio TLS solo
  cuando es ella quien termina la conexión del cliente, lo que exige que al
  llamador se le apunte a Sonda y que confíe en su autoridad. El tráfico que
  solo pasa de camino a otro sitio se reenvía, no se decodifica.
- Los mensajes gRPC comprimidos no se descomprimen.
- La cabecera `Host` se reescribe al upstream, como en cualquier reverse proxy.
- Una captura truncada no se puede reenviar; la negativa es deliberada.
- Los fragmentos de GraphQL no se resuelven: un `...Fields` de primer nivel no
  aporta nombres de campo, porque nombrarlos exigiría el fragmento y el esquema.
- Las capturas tomadas antes del soporte de GraphQL no reportan operación ni
  errores. Nada se vuelve a leer con efecto retroactivo: una captura vieja que
  cambia de resultado en silencio es peor que una que está honestamente vacía.
- Una sentencia preparada en un ciclo y ejecutada en otro posterior muestra el
  nombre con el que se preparó y no su SQL: el texto cruzó el cable una sola
  vez, en la captura que contiene el `Parse`, y no se escriben en una captura
  bytes que no pasaron por ella. Los parámetros, el tiempo y el resultado sí
  están, y los drivers que preparan por consulta y no por conexión no se ven
  afectados.
- Un cliente que encadena más de dieciséis sentencias sin recibir una sola
  respuesta hace que la más antigua se escriba tal como está en vez de quedar
  retenida: nada puede acumularse en memoria esperando una respuesta que no va a
  llegar.
- Una conexión de Postgres que negocia TLS se reenvía y se captura, pero los
  bytes posteriores al handshake son registros TLS y nada en ellos se puede
  leer. Terminarlo no es el mismo trabajo que terminar HTTPS: el cliente pide
  el cifrado con un `SSLRequest` dentro del protocolo y no antes, así que
  Sonda tendría que responder ese byte, mantener una sesión TLS en cada
  dirección y retomar el enmarcado en el segundo mensaje de arranque. Por eso
  `tls: true` se rechaza en un target postgres en vez de aceptarse e
  ignorarse.
- La interfaz no tiene cursores ni trigger, dos dispositivos que un instrumento
  real sí tiene, y el siguiente alcance evidente.
- No se inyecta un id de traza propio. Las peticiones que traen uno se agrupan
  con exactitud; el resto se agrupa por anidamiento y el árbol avisa de que lo
  infirió.

## Desarrollo

```bash
go test ./...
go vet ./...
go run ./cmd/sonda -version   # the commit a binary came from
```

El detector de carreras necesita cgo, y este proyecto deliberadamente no
necesita toolchain de C (el driver de SQLite es Go puro), así que `go test
-race` corre en CI y no en una estación de trabajo Windows. No es un trámite: el
proxy lee el cuerpo de una request en la goroutine del transporte mientras el
handler lee la captura.

Los tests usan archivos SQLite reales, servidores HTTP reales y un cliente y un
servidor gRPC reales; no hay mocks de ninguno.

`PRODUCT.md` y `DESIGN.md` contienen el registro de producto y el sistema
visual. La interfaz es HTML, CSS y JavaScript planos bajo `internal/web/static`,
servidos con `go:embed`; editarla no requiere toolchain, solo recompilar.

Después de cambiar `examples/grpcdemo/proto`, regenera el código Go y el
descriptor set commiteado:

```bash
go install github.com/bufbuild/buf/cmd/buf@latest
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

buf lint
buf generate
buf build -o examples/grpcdemo/descriptors.binpb
```

`buf` es un compilador en Go puro, así que no hace falta instalar `protoc`. Un
test falla si el descriptor set commiteado se desincroniza del código generado.
