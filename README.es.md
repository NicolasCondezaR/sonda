# Sonda

Un proxy que captura el tráfico de desarrollo local. Apunta un cliente a Sonda
en vez de al servicio con el que habla, y las llamadas HTTP, los flujos gRPC,
las sentencias de base de datos y las unidades AMQP que lo cruzan quedan
disponibles para buscar, comparar cuando corresponde y leer tanto por ti como
por un agente de código.

[![CI](https://github.com/NicolasCondezaR/sonda/actions/workflows/ci.yml/badge.svg)](https://github.com/NicolasCondezaR/sonda/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8)](go.mod)
[![Licencia: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

*[Read me in English](README.md)* · **[Documentación](docs/es/README.md)**

![El campo de eventos: un carril por servicio, los fallos como barras de alto completo](docs/assets/sonda-field.jpg)

## Por qué

Depurar entre servicios suele significar leer logs de varios contenedores, y
ninguno contiene el payload. `mitmproxy` resuelve eso para HTTP. Para gRPC el
terreno es más delgado, pero no está vacío: `grpc-tools` y `proxide` lo capturan,
Fiddler lo decodifica, Wireshark lo disecciona. Lo que ninguna hace es sostener
varios protocolos en una sola captura, decodificar protobuf cuando no hay ningún
esquema disponible, y entregar el resultado a un agente de código a través de una
API. Ese es el hueco al que apunta Sonda. El mapa honesto, incluido cuándo
conviene usar otra de esas herramientas, está en
[Trabajo relacionado](docs/es/comparison.md).

## Cómo funciona

Un proxy explícito: un puerto de escucha por servicio observado. Nada se
intercepta a tus espaldas, y un servicio que no está configurado no se captura.

```
cliente ──▶ sonda :9101 ──▶ tu servicio :3000
                 │
                 └──▶ SQLite ──▶ API de consulta :9000
```

Dos propiedades gobiernan el diseño. **El reenvío es exacto byte a byte**: un
depurador que altera el tráfico invalida toda conclusión sacada de él. Y **los
bytes guardados son el registro**: los cuerpos se guardan tal como cruzaron el
cable y se decodifican solo al mostrarlos, así el replay sigue significando algo
y una captura vieja se vuelve legible cuando aparece su esquema. Las dos
excepciones deliberadas —contraseñas de PostgreSQL e intercambios SASL de AMQP—
se blanquean antes de escribir nada. Ver
[Almacenamiento, comportamiento y costo](docs/es/storage.md).

## Qué captura

| | |
|---|---|
| **HTTP/1.1 y HTTP/2** | Peticiones, respuestas, cabeceras, cuerpos |
| **gRPC** | Unario y streaming, trailers preservados, protobuf decodificado — por reflection, por descriptor set, o [estructuralmente desde el wire format cuando no existe ninguno](docs/es/protocols.md#grpc) |
| **WebSocket y server-sent events** | Frame por frame, [handshake reenviado literalmente](docs/es/protocols.md#sockets-y-flujos-de-eventos) |
| **GraphQL** | La [operación leída del cuerpo](docs/es/protocols.md#graphql), para que dos POST idénticos dejen de ser indistinguibles |
| **PostgreSQL** | [El protocolo de cable](docs/es/protocols.md#postgresql): sentencias, parámetros, resultados |
| **AMQP 0-9-1 y AMQPS** | [Publicaciones, entregas y confirmaciones](docs/es/protocols.md#amqp-0-9-1) como unidades de trabajo |
| **TLS** | [Una CA local que no instala nada](docs/es/protocols.md#tls), para los servicios que solo hablan HTTPS |

## Instalación

Elige la que ya tengas. Nada de esto necesita un toolchain de C ni un SQLite del
sistema: el driver es Go puro, que es también la razón por la que los binarios
son estáticos y la imagen pesa 50 MB.

| | |
|---|---|
| **macOS** | `brew install NicolasCondezaR/tap/sonda` |
| **Windows** | `scoop bucket add nicolascondezar https://github.com/NicolasCondezaR/scoop-bucket`<br>`scoop install sonda` |
| **Go** | `go install github.com/NicolasCondezaR/sonda/cmd/sonda@latest` |
| **Docker** | `docker run -p 127.0.0.1:9000:9000 -v sonda:/data ghcr.io/nicolascondezar/sonda` |
| **Binario** | Descárgalo de [Releases](https://github.com/NicolasCondezaR/sonda/releases), descomprime, ejecuta |

Todo escucha en `127.0.0.1` a propósito: la captura contiene lo que cruzó el
cable. El detalle completo, las notas de Linux y las particularidades de
PowerShell están en [Instalación](docs/es/install.md).

## Inicio rápido

```bash
docker compose up -d
```

Eso levanta Sonda más dos servicios de juguete para tener algo que capturar: un
`echo` HTTP y un `grpcdemo` gRPC. Sus puertos propios no se publican a propósito
—el tráfico debe llegar a través del proxy.

Abre **http://127.0.0.1:9000** y mándale algo:

```bash
curl http://127.0.0.1:9101/ok                 # HTTP, por Sonda
curl 'http://127.0.0.1:9101/fail?status=503'  # un fallo, para verlo marcado
grpcurl -plaintext -d '{"order_id":"ORD-1"}' \
  127.0.0.1:9201 demo.v1.Orders/GetOrder      # gRPC, por Sonda
```

Sin Docker, las mismas tres peticiones llegan a los mismos puertos:

```bash
go build -o sonda ./cmd/sonda
go build -o echo ./examples/echo
go build -o grpcdemo ./examples/grpcdemo

cp sonda.example.yaml sonda.yaml
./echo -addr 127.0.0.1:8081 &
./grpcdemo -addr 127.0.0.1:8082 &
./sonda -config sonda.yaml
```

Apuntarla a tus propios servicios es cuestión de nombrarlos una vez: Sonda puede
leer el `.env` o el compose de un proyecto en vez de que escribas quince
servicios a mano. Ver [Proyectos y configuración](docs/es/configuration.md), y
[No captura nada, ahora qué](docs/es/troubleshooting.md) si un puerto queda
callado.

## Para agentes de código

Sonda incluye un servidor MCP, así que un agente que depura tu código puede
preguntar qué cruzó el cable de verdad en vez de pedirte que pegues logs:

```
recent_failures      qué acaba de romperse
diff_calls           esta funcionó y esta no — qué cambió
diff_flows           dos corridas completas alineadas, y dónde se separaron
arm_trigger          atrapa la próxima llamada que cruce una condición, dentro de horas
wait_for_call        dispara algo y después verifica qué salió por el cable
trace_call           el árbol de llamadas que provocó una petición
```

Las credenciales nunca vuelven: las cabeceras de autorización, las cookies y
campos similares se redactan antes de que la respuesta salga del proceso, y eso
no se puede desactivar. La interfaz web, el cliente de terminal y el servidor
MCP son todos clientes de la misma [API HTTP](docs/es/api.md). Ver
[Agentes de código](docs/es/agents.md).

## Más allá de la captura

La captura es el piso. Sobre los bytes grabados:

- **[Replay y diff](docs/es/replay.md)** — reenviar una llamada guardada, o
  comparar dos de forma estructural para ver qué difiere realmente.
- **[Comparar dos corridas](docs/es/replay.md#comparar-dos-corridas)** — alinear
  un flujo que funcionó con uno que no, y nombrar la primera llamada donde se
  separaron. Los ids en las rutas no impiden que dos corridas emparejen.
- **[Modo stub](docs/es/experiments.md#modo-stub)** — responder por un servicio
  desde sus propias grabaciones en vez de llamarlo.
- **[Romper a propósito](docs/es/experiments.md)** — latencia, estados forzados,
  conexiones cortadas.
- **[Deriva de contratos](docs/es/experiments.md#deriva-de-contratos)** — qué
  empezó a mandar un servicio que su esquema nunca prometió.
- **[El trigger](docs/es/experiments.md#el-trigger)** — arma una condición y anda
  a hacer otra cosa; vuelve al momento en que disparó. Nunca coincide hacia
  atrás, y nunca le quita la vista a quien ya está leyendo.

## Estado

Fase 20 completa: captura, decodificación, almacenamiento, búsqueda, la API de
consulta, la interfaz web, replay, diff estructural, un cliente de terminal,
gestión de proyectos, árboles de llamadas, modo stub, inyección de fallos,
deriva de contratos, el servidor MCP, TLS, AMQP 0-9-1, el diff de flujos y el trigger funcionan, y todo el
conjunto corre con `docker compose up`. Ver la
[hoja de ruta](docs/es/roadmap.md).

## Contribuir

Los tests usan lo real: archivos SQLite reales, servidores HTTP reales, un
cliente y servidor gRPC reales. Ver [CONTRIBUTING.md](CONTRIBUTING.md), y
[SECURITY.md](SECURITY.md) para cualquier cosa que no deba reportarse en
público.

MIT. Ver [LICENSE](LICENSE).
