[← Docs](README.md)

# Interfaces

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
- **Cursores.** Dos, `A` y `B`, como los rotula el instrumento al que esto se
  parece. Selecciona una llamada y presiona `a` o `b` —o haz clic en el
  control— y una línea de un píxel cruza todos los canales en el lugar que esa
  llamada ocupa en el tiempo; con los dos puestos, la barra lee el intervalo
  entre ellos. La misma tecla lo levanta.
- **Inspector.** La llamada seleccionada, decodificada, indicando de dónde salió
  el esquema.

Arranca filtrada a fallos, porque ese es el motivo por el que la abriste. `ALL`
cambia al campo completo. Dejar el puntero sobre el campo **congela el trazo**,
para que una marca deje de deslizarse mientras le apuntas; al salir, se reanuda.

Un cursor se ancla a una **llamada**, no a una posición en pantalla. El campo es
una ventana que se desplaza sola, así que un cursor parado en una x mediría la
conversión pixel→tiempo de la vista y no el tráfico: cada intervalo que lee es
la diferencia entre dos timestamps grabados, y sigue siendo el mismo número
cuando cambias la ventana. La lectura es de inicio a inicio —la duración de
cada llamada ya está en pantalla como el ancho de su marca— y la flecha dice
cuál de los dos cursores es el más temprano, así que nunca hay un intervalo
negativo que interpretar. Un cursor cuya llamada se sale de la ventana se
levanta, en vez de quedar apuntando a nada.

`FIND` busca en rutas y en el texto de los payloads, incluidos los que Sonda
solo tiene como bytes. `/` enfoca el buscador y `Escape` cierra el inspector.

La interfaz completa va embebida en el binario: sin Node, sin paso de build, sin
peticiones de red y sin fuentes web.

![Un fallo gRPC: protobuf decodificado por reflection, con el estado real](../assets/sonda-grpc-inspector.jpg)

Arriba: una llamada gRPC que devolvió `PermissionDenied`. El estado HTTP es 200
—gRPC reporta el fallo por debajo de HTTP— y la request aparece decodificada con
nombres de campo porque el servicio sirve reflection.

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

La traducción es casi directa: la monoespaciada es gratuita aquí, las líneas de un
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
| `a` / `b` | poner un cursor de medición sobre la llamada seleccionada, o levantarlo; con los dos puestos la barra lee el intervalo entre ellos |
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

