[← Docs](README.md)

# Trabajo relacionado

Sonda no es la primera herramienta que captura gRPC. Quien la evalúe merece el
mapa antes del argumento, incluidos los casos en los que otra cosa es la mejor
respuesta.

| Herramienta | Qué es | Dónde se solapa | Dónde se detiene |
|---|---|---|---|
| [`bradleyjkemp/grpc-tools`](https://github.com/bradleyjkemp/grpc-tools) | `grpc-dump`, `grpc-replay`, `grpc-fixture` — CLIs en Go | La más cercana de todas: captura, replay y respuestas grabadas, las mismas tres ideas | Solo gRPC, la salida es un flujo JSON en disco, sin API de consulta, sin interfaz, sin otro protocolo al lado |
| [`Rantanen/proxide`](https://github.com/Rantanen/proxide) | Proxy depurador HTTP/2 y gRPC con TUI en Rust | Captura a archivo y decodifica con descriptors | Sin API, así que nada más puede leer la captura; un solo protocolo |
| [Fiddler Everywhere](https://www.telerik.com/fiddler/fiddler-everywhere/documentation/capture-traffic/advanced-capturing-options/capturing-grpc-traffic) | Depurador comercial con GUI | Decodifica gRPC por reflection o archivos `.proto` | Comercial, GUI primero, la captura HTTP/2 se activa a mano; pensado para una persona leyendo una llamada |
| [HTTP Debugger](https://www.httpdebugger.com/debug/grpc-calls) | Comercial, Windows | Lee HTTP/2, el framing de protobuf y los trailers `grpc-status` del cable, sin proxy | Solo Windows, comercial, una máquina |
| [Wireshark + dissectors gRPC/protobuf](https://grpc.io/blog/wireshark/) | Analizador de protocolos de red | Ve los frames, con esquemas si le das alguno | Es un analizador: sin replay, sin stub, sin inyección de fallos, sin correlación entre una llamada y las que provocó |
| [Kubeshark](https://github.com/kubeshark/kubeshark) | Observabilidad de red con eBPF para Kubernetes | Multi-protocolo — HTTP, gRPC, Kafka, Redis, AMQP, DNS — y también expone MCP a agentes | Es Kubernetes. Quiere un cluster, un DaemonSet y eBPF; no es lo que corres contra tres servicios en un notebook |

## Lo que hace Sonda que ninguna hace junto

- **Una sola captura entre protocolos.** HTTP, gRPC, WebSocket, server-sent
  events, operaciones GraphQL, sentencias PostgreSQL y unidades AMQP caen en el
  mismo almacén y se buscan lado a lado, porque un bug entre servicios rara vez
  se queda dentro de un protocolo.
- **Decodifica protobuf sin ningún esquema.** Primero reflection, después el
  descriptor set del proyecto, y cuando no existe ninguno de los dos — el caso
  normal en un monorepo cuyos servicios no sirven reflection — decodifica el
  wire format de forma estructural en vez de rendirse.
- **La API es el producto y las interfaces son sus clientes.** La interfaz web,
  el cliente de terminal y el [servidor MCP](agents.md) leen la misma API HTTP.
  Un agente de código es un consumidor de primera clase, no un formato de
  exportación.
- **La captura es el piso, no el techo.** [Replay y diff
  estructural](replay.md), [modo stub, inyección de fallos y deriva de
  contratos](experiments.md) trabajan sobre los bytes que se grabaron de verdad.
- **Un binario estático y un archivo SQLite.** Sin cluster, sin eBPF, sin
  toolchain de C, sin demonio que mantener vivo. `brew install`, o
  `docker compose up`.

## Cuándo usar otra cosa

- **Solo necesitas gRPC y solo necesitas un dump.** `grpc-tools` es más chica y
  hace eso bien.
- **Tu tráfico vive en un cluster de Kubernetes.** Kubeshark está hecha para
  eso y Sonda no: Sonda es un proxy explícito por puerto para desarrollo local.
- **Quieres la verdad a nivel de paquete sobre una red que no controlas.** Eso
  es Wireshark, y siempre lo será.
- **Quieres una GUI comercial pulida con soporte detrás.** Fiddler.

Sonda apunta al caso que quedó afuera: varios servicios en una máquina,
protocolos mezclados, sin esquemas a mano, y un agente de código que debería
poder leer lo que cruzó el cable sin que una persona pegue logs en un prompt.
