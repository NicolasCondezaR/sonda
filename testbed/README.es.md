# El banco de pruebas: una librería que funciona a medias

Un sistema pequeño para que Sonda lo observe, y un recorrido que ejercita cada
capacidad contra él.

Los propios `examples/echo` y `examples/grpcdemo` de Sonda alcanzan para ver que
aparece una captura. No alcanzan para ver un árbol de petición, una sentencia
colgando debajo de la llamada HTTP que la ejecutó, un contrato que derivó, un
fallo de gRPC escondido bajo un HTTP 200, o una credencial que la interfaz web
muestra y el servidor MCP no. Este es un sistema que produce todo eso, de forma
continua, con el tráfico sano y el tráfico roto en el campo al mismo tiempo.

**El contraste es el punto.** Más o menos dos tercios de lo que fluye por aquí
funciona y un tercio falla, lado a lado, así un lector puede ver cómo se ve una
llamada que funciona junto a una rota y distinguirlas de un vistazo.

*[Read it in English](README.md).*

---

## Contenidos

- [La librería](#la-librería)
- [Levantarlo](#levantarlo)
- [Qué está fluyendo](#qué-está-fluyendo)
- [Los interruptores](#los-interruptores)
- [Antes de empezar los ejercicios](#antes-de-empezar-los-ejercicios)
- **Los ejercicios**
  - [1. ¿Se está capturando algo?](#1-se-está-capturando-algo)
  - [2. El campo: lo sano y lo roto, lado a lado](#2-el-campo-lo-sano-y-lo-roto-lado-a-lado)
  - [3. Búsqueda](#3-búsqueda)
  - [4. Los fallos que un código de estado no vería](#4-los-fallos-que-un-código-de-estado-no-vería)
  - [5. El árbol de la petición](#5-el-árbol-de-la-petición)
  - [6. El mismo árbol con un id de petición](#6-el-mismo-árbol-con-un-id-de-petición)
  - [7. PostgreSQL](#7-postgresql)
  - [8. Deriva de contratos](#8-deriva-de-contratos)
  - [9. Diff](#9-diff)
  - [10. Replay](#10-replay)
  - [11. Modo stub](#11-modo-stub)
  - [12. Romper cosas a propósito](#12-romper-cosas-a-propósito)
  - [13. Un servicio que simplemente está caído](#13-un-servicio-que-simplemente-está-caído)
  - [14. WebSocket y server-sent events](#14-websocket-y-server-sent-events)
  - [15. TLS](#15-tls)
  - [16. ¿Por qué no veo nada?](#16-por-qué-no-veo-nada)
  - [17. La superficie MCP](#17-la-superficie-mcp)
  - [18. Las credenciales no salen por MCP](#18-las-credenciales-no-salen-por-mcp)
  - [19. El cliente de terminal](#19-el-cliente-de-terminal)
- [Detenerlo](#detenerlo)
- [Qué no cubre este banco de pruebas](#qué-no-cubre-este-banco-de-pruebas)

---

## La librería

Cinco servicios y una base de datos. Cada flecha de este diagrama es un puerto de
Sonda: ningún servicio conoce la dirección de otro servicio, solo la dirección
del proxy que tiene delante.

```
                      ┌──────────────────────────────────────────┐
     tú / driver ──▶ 9401 gateway                                 │
                      │                                           │
                      ├─▶ 9402 catalog ──▶ 9406 catalog-db ──▶ postgres
                      ├─▶ 9404 pricing        (gRPC, sin reflection)
                      ├─▶ 9403 storefront     (GraphQL, WebSocket, SSE)
                      └─▶ 9405 shipping       (apagado, a propósito)
                      └──────────────────────────────────────────┘

                        La interfaz de la propia Sonda: 9400
```

| Puerto | Canal | Qué es |
|---|---|---|
| `9400` | — | La interfaz web de Sonda, su API de consulta y su endpoint MCP |
| `9401` | `gateway` | HTTP. La puerta de entrada, y el único servicio que llama a otros servicios |
| `9402` | `catalog` | HTTP. Libros y socios. El único servicio que ejecuta SQL |
| `9403` | `storefront` | HTTP. GraphQL, un WebSocket y un flujo de eventos |
| `9404` | `pricing` | gRPC, **sin servir reflection** |
| `9405` | `shipping` | HTTP. Declarado, deliberadamente sin correr |
| `9406` | `catalog-db` | PostgreSQL |
| `9407` | `storefront-tls` | El mismo storefront detrás de un listener TLS |
| `8101` | — | El puerto propio del catalog, saltándose Sonda. Solo endpoint de control |
| `8102` | — | El puerto propio del storefront, lo mismo |

Nada de esto choca con el `compose.yaml` del propio repositorio, que ocupa el
`9000`, el `9101` y el `9201`. **Los dos pueden correr al mismo tiempo**, y vale
la pena hacerlo una vez solo para ver que dos Sondas con dos bases de datos no
se interfieren.

El dominio es una librería: `DUNE`, `PALE-FIRE` y `SOLARIS` están en stock;
`RESTRICTED-1` está en la colección reservada; `KAPUT` es un SKU que rompe el
catalog a propósito.

## Levantarlo

```bash
cd testbed
docker compose up -d
```

Después abre **http://127.0.0.1:9400**.

En unos veinte segundos el campo ya tiene tráfico, y como un tercio de ese
tráfico es rojo. Sonda arranca filtrada a fallos, y por eso lo primero que ves
son las fallas; `ALL` cambia a todo.

Para mirar qué están haciendo los clientes de la librería:

```bash
docker compose logs -f driver
```

## Qué está fluyendo

El servicio `driver` corre los mismos dieciocho pasos cada veinte segundos. Son
deliberadamente los mismos dieciocho: un ejercicio que dice "mira el tercero"
tiene que poder decirlo en serio. Nada de esto es aleatorio.

```
── cycle 4 ──
   checkout DUNE                                                          ok
   checkout PALE-FIRE x5                                                  ok
   checkout RESTRICTED-1 (gRPC PermissionDenied under HTTP 200)           FAILED
   checkout SOLARIS for a customer that does not exist (GraphQL errors …) FAILED
   checkout DUNE express (shipping is not running)                        FAILED
   checkout KAPUT (an ordinary HTTP 500 from the catalog)                 FAILED
   report 400ms (slow, succeeds)                                          ok
   report 2500ms (the gateway gives up at 1s)                             FAILED
   reviews for DUNE                                                       ok
   reviews from a table that does not exist (SQL error, no status code)   FAILED
   search the catalog                                                     ok
   log in (password in a JSON body and in a SQL literal)                  ok
   oauth callback (credentials in the query string)                       ok
   websocket, closed cleanly                                              ok
   websocket, closed with 1011 and a reason                               FAILED
   server-sent events                                                     ok
   gRPC server stream                                                     ok
   graphql batch                                                          ok
```

Ahí dentro hay siete tipos distintos de fallo, y son distintos a propósito,
porque Sonda los trata distinto y se ven distinto:

| Tipo | De dónde viene | Qué dice el transporte |
|---|---|---|
| Un 5xx corriente | `GET /books/KAPUT` | HTTP 500 |
| Un estado de gRPC | `Quote` para un SKU `RESTRICTED-*` | **HTTP 200** |
| Errores de GraphQL | cualquier operación para el cliente `ghost` | **HTTP 200** |
| Un error de SQL | `GET /reviews?broken=1` | **ningún código de estado** |
| Un servicio caído | cualquier checkout express | HTTP 502 de Sonda, con el error de conexión |
| Un timeout | `GET /report?ms=2500` | quien llama deja de esperar; sin estado |
| Un socket que cierra mal | el WebSocket al que se le dice `boom` | HTTP 101, código de cierre 1011 — **y Sonda no lo marca** |

Los tres en negrita son los que vale la pena demostrar a propósito: **una
herramienta que lee solo el código de estado informa los tres como éxitos.**

La última fila es la excepción honesta, y el [ejercicio 14](#14-websocket-y-server-sent-events)
vuelve sobre ella: el código de cierre y su motivo están en la captura, pero un
socket que termina mal no se cuenta como fallo del modo en que sí se cuentan un
estado de gRPC o un error de GraphQL. El log del driver llama a ese paso `FAILED`
porque el driver leyó el código de cierre; el filtro de fallos de Sonda no.

Y dos cosas que no son fallos pero que conviene distinguir de uno:

- `GET /report?ms=400` es **lento y está bien** — una marca ancha en el campo, no
  una barra de fallo. La latencia es un problema distinto del fallo.
- Cada checkout que falla lo hace **como una rama de una petición cuyas otras
  ramas funcionaron**. Esa es la forma que tiene una caída real.

## Los interruptores

Los fallos que dependen de la petición no necesitan interruptor y están fluyendo
siempre. Dos cosas son estado y no petición, así que son interruptores — y son
interruptores en vez de probabilidades por la misma razón por la que la inyección
de fallos de Sonda cuenta llamadas en vez de tirar los dados: un ejercicio que
dice "ahora rómpelo y mira de nuevo" tiene que dar la misma respuesta dos veces.

```bash
# el contrato del catalog deriva: un campo desaparece, otro cambia de tipo
curl -X POST http://127.0.0.1:8101/_control -H 'Content-Type: application/json' \
  -d '{"drift":true}'

# el storefront responde GraphQL con una página HTML 502 en vez de JSON
curl -X POST http://127.0.0.1:8102/_control -H 'Content-Type: application/json' \
  -d '{"graphql_down":true}'

# qué está puesto en este momento
curl -s http://127.0.0.1:8101/_control
```

Esos van al puerto **propio** del servicio, no a través de Sonda. Accionar un
interruptor es tramoya, y una captura suya en medio del campo es ruido dentro del
ejercicio que estaba preparando.

## Antes de empezar los ejercicios

**Detén el driver.** Los ejercicios de abajo hacen una llamada y después la
miran; el tráfico en vivo de fondo convierte "la última llamada" en un blanco
móvil, y además mete llamadas ajenas dentro de la ventana de tiempo con la que se
arma el árbol de petición.

```bash
docker compose stop driver     # and `docker compose start driver` when you are done
```

**Estos comandos son shell de Linux/macOS.** En PowerShell 5.1 las comillas
dentro de `-d '{"a":"b"}'` se reescriben antes de que `curl.exe` las vea y el
cuerpo llega corrompido. Usa `Invoke-RestMethod`, o pon el cuerpo en un archivo y
usa `--data-binary '@body.json'`. El README del propio repositorio tiene una
sección sobre esto.

**`jq` es opcional.** Todos los comandos de abajo funcionan sin él; solo está
para hacer legible el JSON. Quita el `| jq …` y obtienes la misma respuesta en
una línea.

**Algunos ejercicios cambian estado.** Cada uno dice cómo dejarlo como estaba. Si
pierdes la cuenta, `docker compose restart sonda` olvida todo stub y toda regla
de fallo —ninguno de los dos se escribe nunca en la base de datos— y los
interruptores de control vuelven a apagado cuando su servicio reinicia.

---

# Los ejercicios

## 1. ¿Se está capturando algo?

**Ejecuta**

```bash
curl -s http://127.0.0.1:9400/api/stats
```

**Cómo se ve un resultado correcto**

```json
{"calls":113,"oldest":"…","newest":"…","dropped":0,
 "by_target":[{"target":"catalog","calls":28,"faults":6,…}, …]}
```

Seis o siete targets, un conteo distinto de cero en cada uno, y `dropped: 0`.

**Vale la pena saberlo.** `dropped` son capturas descartadas porque el buffer de
escritura estaba lleno. La captura nunca demora el tráfico —ante una ráfaga Sonda
descarta en vez de frenar el sistema que está observando—, así que este número es
la forma de enterarse de que pasó, en vez de quedarse calladamente con una imagen
incompleta.

---

## 2. El campo: lo sano y lo roto, lado a lado

**Mira** http://127.0.0.1:9400

**Cómo se ve un resultado correcto.** Un carril por canal contra un eje de tiempo
vivo. Vuelve a arrancar el driver (`docker compose start driver`) y mira llegar
un ciclo: `catalog` y `gateway` se llenan de marcas, y cada veinte segundos
aparece un racimo de **barras de alto completo** — los fallos. Un fallo es una
*forma* distinta, no solo un color distinto, que es lo que hace que sobreviva a
un canal rojo y a un lector daltónico.

Presiona `ALL` para ver también los éxitos. El contraste es lo que importa: el
mismo carril de `gateway` lleva los dos checkouts que funcionaron y los cuatro
que no, en el mismo segundo.

Deja el puntero sobre el campo y el trazo **se congela**, para que una marca deje
de deslizarse mientras le apuntas.

**Vale la pena saberlo.** Los conteos del riel de canales están sin filtrar a
propósito. El riel responde "¿está sano este servicio?", así que poner un filtro
delante del campo no puede cambiarlo.

Detén el driver otra vez antes de seguir.

---

## 3. Búsqueda

**Ejecuta**

```bash
# por canal
curl -s 'http://127.0.0.1:9400/api/calls?target=pricing&limit=3'

# por texto en los payloads, incluidos los que Sonda solo tiene como bytes
curl -s 'http://127.0.0.1:9400/api/calls?q=RESTRICTED-1&limit=3'

# por subcadena de la ruta
curl -s 'http://127.0.0.1:9400/api/calls?path=/books/&limit=3'
```

**Cómo se ve un resultado correcto.** `q=RESTRICTED-1` encuentra el checkout del
gateway, la consulta del catalog **y la sentencia SQL que está debajo** — tres
canales y dos protocolos, calzados por una cadena que estaba dentro de los
cuerpos:

```
188 gateway     http      /checkout
186 catalog     http      /books/RESTRICTED-1
185 catalog-db  postgres  bookshop
```

**Vale la pena saberlo.** `q` es una frase literal, no un lenguaje de consulta,
así que `q={"sku":"DUNE"}` funciona tal cual se escribe. `path` calza por
subcadena; `q` busca además en los payloads — incluido el SQL, sus parámetros
asociados y la queja del servidor, no solo la línea de resumen.

La llamada a `pricing` **no** está en esa lista aunque el SKU venía en su
request, porque un mensaje protobuf es binario y los payloads genuinamente
binarios no se indexan. Busca una llamada gRPC por `target`, `path` o
`grpc_status` en su lugar.

---

## 4. Los fallos que un código de estado no vería

Este es el ejercicio que justifica la herramienta.

**Ejecuta**

```bash
curl -s 'http://127.0.0.1:9400/api/calls?failed=true&limit=12'
```

**Cómo se ve un resultado correcto** — doce filas en las que la columna `status`
por sí sola estaría mintiendo sobre al menos tres de ellas:

```
209  catalog     http      GET  /reviews?sku=DUNE&broken=1   status=502  gql=0  pg=0
208  catalog-db  postgres  STATEMENT bookshop                status=0    gql=0  pg=1
205  gateway     http      GET  /report?ms=2500              status=504  gql=0  pg=0
204  catalog     http      GET  /slow?ms=2500                status=502  gql=0  pg=0   context canceled
201  gateway     http      POST /checkout                    status=502  gql=0  pg=0
200  catalog     http      GET  /books/KAPUT                 status=500  gql=0  pg=0
198  shipping    http      POST /quote                       status=502  gql=0  pg=0   dial tcp: lookup shipping…
192  storefront  http      POST /graphql                     status=200  gql=1  pg=0
187  pricing     grpc      POST /shop.v1.Pricing/Quote       status=200  gql=0  pg=0   grpc=7
```

Lee las tres que importan:

- **`187` es HTTP 200 y es un fallo.** gRPC reporta por debajo de HTTP: el estado
  está en los trailers, y es `7 PermissionDenied`. Toda herramienta de gRPC que
  filtre por estado HTTP muestra esta llamada como sana.
- **`192` es HTTP 200 y es un fallo.** GraphQL responde 200 con un arreglo
  `errors`. El mismo problema, la misma respuesta: Sonda lo cuenta como fallo en
  el filtro, en el riel, en el campo, en la terminal, en el árbol y en
  `recent_failures`.
- **`208` no tiene ningún código de estado.** Un error de SQL es un
  `ErrorResponse` dentro del flujo. No hay ninguna señal a nivel de transporte en
  ninguna parte.

**Ahora mira una de cada una.**

```bash
# la de gRPC: HTTP 200, gRPC 7, y el mensaje decodificado del percent-encoding
curl -s 'http://127.0.0.1:9400/api/calls?target=pricing&failed=true&limit=1'
```

```json
{"id":187,"target":"pricing","protocol":"grpc","method":"POST",
 "path":"/shop.v1.Pricing/Quote","status":200,
 "grpc_status":7,"grpc_status_text":"PermissionDenied",
 "grpc_message":"this title is held in the reserve collection — ask a librarian"}
```

`status` y `grpc_status` están los dos en el listado, uno al lado del otro,
porque mostrar uno sin el otro es como sobrevive esta clase de bug.

```bash
# la de GraphQL, completa
curl -s 'http://127.0.0.1:9400/api/calls?target=storefront&failed=true&limit=1'
```

```json
{"id":192,"status":200,"graphql_op":"query CustomerOrders","graphql_errors":1}
```

La operación va en la fila. Sin eso, un servicio GraphQL llega al campo como un
único endpoint llamado doscientas veces y la línea de tiempo deja de servir para
él. Abre la llamada misma y el inspector tiene la operación, las variables que se
le enviaron, y cada error con su ruta y su `extensions.code`:

```bash
ID=$(curl -s 'http://127.0.0.1:9400/api/calls?target=storefront&failed=true&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/calls/$ID" | jq .graphql
```

```json
{"batch": false,
 "operations": [{"type":"query","name":"CustomerOrders","label":"query CustomerOrders",
   "fields":["customer"], "variables":{"id":"ghost"},
   "errors":[{"message":"no customer with that id","path":"customer","code":"NOT_FOUND"}]}],
 "errors": 1}
```

**Vale la pena saberlo.** El documento GraphQL se detecta por el **cuerpo**, no
por la ruta — un POST cuyo JSON lleva una cadena `query` es una petición GraphQL
vaya a donde vaya. Y un batch es todas las operaciones que contiene: el driver
manda uno en cada ciclo, y vuelve etiquetado como
`batch of 2: query Shelf, query CustomerOrders`.

**"Cero errores" y "no se pudo saber" son respuestas distintas.** Acciona el otro
interruptor del storefront y el servicio responde una página HTML de error en vez
de JSON:

```bash
curl -s -X POST http://127.0.0.1:8102/_control -H 'Content-Type: application/json' -d '{"graphql_down":true}'
curl -s -o /dev/null -w '%{http_code} %{content_type}\n' -X POST http://127.0.0.1:9403/graphql \
  -H 'Content-Type: application/json' -d '{"query":"query Shelf { shelf }"}'
ID=$(curl -s 'http://127.0.0.1:9400/api/calls?target=storefront&path=/graphql&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/calls/$ID" | jq .graphql
curl -s -X POST http://127.0.0.1:8102/_control -H 'Content-Type: application/json' -d '{"graphql_down":false}'
```

```
502 text/html; charset=utf-8
```

```json
{"batch":false,
 "operations":[{"type":"query","name":"Shelf","label":"query Shelf","fields":["shelf"]}],
 "errors":0,"unreadable":true}
```

La operación sigue teniendo nombre —eso vino de la *request*— y la respuesta
queda marcada como **`unreadable`** en vez de informarse como una llamada sin
errores. Sonda lee del documento lo justo para etiquetarlo y nada más; no es un
parser, no valida, y no conoce tu esquema. Una respuesta cortada por el tope de
cuerpo, o una página de error de algo que está delante del servicio, recibe esa
misma respuesta honesta.

---

## 5. El árbol de la petición

Lo más valioso que se puede llegar a ver, y lo más difícil de sacar de los logs.

**Ejecuta**

```bash
# un checkout que funciona
curl -s -X POST http://127.0.0.1:9401/checkout \
  -H 'Content-Type: application/json' \
  -d '{"sku":"DUNE","quantity":2,"customer":"cust-9"}' > /dev/null

# el id de esa llamada
ID=$(curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&limit=1' | jq '.calls[0].id')

curl -s "http://127.0.0.1:9400/api/trace?call=$ID" | jq -r .rendered
```

**Cómo se ve un resultado correcto**

```
(grouped by timing, not by a trace id — the shape is inferred)
gateway /checkout                                              4ms ✓
├─ catalog /books/DUNE                                         1ms ✓
│  └─ catalog-db bookshop                                      0ms ✓
├─ pricing /shop.v1.Pricing/Quote                              1ms ✓
└─ storefront /graphql                                         0ms ✓
```

Cuatro servicios y una base de datos, en una sola petición, ordenados por quién
contuvo a quién. **La sentencia SQL es hija de la llamada HTTP que la ejecutó** —
que es toda la razón de que una captura de Postgres sea una sentencia y no una
conexión: detrás de un pool, una conexión lleva horas del SQL de una aplicación,
y horas de SQL no es algo que se pueda colgar de una petición.

**Ahora la misma petición con una rama que falló:**

```bash
curl -s -X POST http://127.0.0.1:9401/checkout \
  -H 'Content-Type: application/json' \
  -d '{"sku":"DUNE","quantity":1,"customer":"cust-1","express":true}' > /dev/null

ID=$(curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/trace?call=$ID" | jq -r .rendered
```

```
gateway /checkout                                           1005ms ✗
├─ catalog /books/DUNE                                         1ms ✓
│  └─ catalog-db bookshop                                      0ms ✓
├─ pricing /shop.v1.Pricing/Quote                              0ms ✓
├─ storefront /graphql                                         0ms ✓
└─ shipping /quote                                          1000ms ✗
       dial tcp: lookup shipping on 127.0.0.11:53: … i/o timeout
```

Tres ramas funcionaron, una no, y la que falló es lo último que hizo la petición.
Esa es la lectura que un log no te puede dar: el fallo no es una línea roja
aislada, es el cuarto paso de una operación cuyos primeros tres estuvieron bien —
y los 1005 ms dicen que la petición entera se fue en esperarlo.

Prueba los otros checkouts que fallan y lee la forma de cada uno:

```bash
# falla en pricing, por gRPC, bajo un HTTP 200
-d '{"sku":"RESTRICTED-1","quantity":1,"customer":"cust-1"}'
# falla en el storefront, por GraphQL, bajo un HTTP 200
-d '{"sku":"SOLARIS","quantity":1,"customer":"ghost"}'
# falla en el catalog, con un 500 corriente
-d '{"sku":"KAPUT","quantity":1,"customer":"cust-1"}'
```

**Vale la pena saberlo.** La línea de encabezado dice que la agrupación fue
**inferida**. Nada en la librería envía un trace id, así que Sonda los ordenó por
contención: una llamada que no empieza antes ni termina después que otra es hija
suya. Eso es una conjetura, y Sonda lo dice en vez de presentarlo como un hecho.
También es la razón de que el driver corra sus pasos de a uno — dos peticiones
solapadas volverían el anidamiento genuinamente ambiguo, y Sonda lo marcaría
honestamente como `ambiguous`.

---

## 6. El mismo árbol con un id de petición

**Ejecuta**

```bash
curl -s -X POST http://127.0.0.1:9401/checkout \
  -H 'Content-Type: application/json' \
  -H 'X-Request-Id: shelf-audit-1' \
  -d '{"sku":"DUNE","quantity":2,"customer":"cust-9"}' > /dev/null

ID=$(curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/trace?call=$ID" | jq '{certain: .trace.certain, trace_id: .trace.trace_id, calls: .trace.calls}'
curl -s "http://127.0.0.1:9400/api/trace?call=$ID" | jq -r .rendered
```

**Cómo se ve un resultado correcto**

```json
{"certain": true, "trace_id": "shelf-audit-1", "calls": 4}
```

```
gateway /checkout                                              4ms ✓
├─ catalog /books/DUNE                                         1ms ✓
├─ pricing /shop.v1.Pricing/Quote                              1ms ✓
└─ storefront /graphql                                         0ms ✓
```

El gateway reenvía el id de petición que le entregaron — a los saltos HTTP como
cabecera y al salto gRPC como metadata, que sobre HTTP/2 es lo mismo — así que
Sonda agrupa por el id y el aviso de "inferido" desapareció. `certain: true`.

**Vale la pena saberlo, y aquí está la sutileza del ejercicio: la sentencia SQL
ya no está en el árbol.** Cuatro llamadas, no cinco. Un trace id agrupa por
*exactamente* ese id, y una sentencia de Postgres no tiene dónde llevar uno — el
protocolo no tiene cabeceras. Así que propagar un trace id compra certeza para
los saltos que pueden llevarlo y pierde el salto que no puede.

Las dos lecturas son ciertas y las dos sirven. Manda un id cuando quieras saber
con certeza qué llamadas fueron juntas; no mandes ninguno cuando quieras la base
de datos que hay debajo.

El gateway solo reenvía un id que llegó y nunca inventa uno, que es lo que hace
un gateway de verdad — y lo que permite que este banco de pruebas muestre ambas
cosas.

---

## 7. PostgreSQL

**Ejecuta**

```bash
curl -s http://127.0.0.1:9402/reviews?sku=DUNE > /dev/null
curl -s 'http://127.0.0.1:9400/api/calls?protocol=postgres&limit=5' \
  | jq -r '.calls[] | "\(.id)  \(.method)  \(.path)  |  \(.postgres_summary)"'
```

**Cómo se ve un resultado correcto**

```
215  STATEMENT  bookshop  |  SELECT stars, body FROM reviews WHERE sku = $1 ORDER BY id -> SELECT 2
211  STATEMENT  bookshop  |  SELECT sku, title, author, price_cents, discount_pct, in_stock FROM books WHERE sku = $1 -> SELECT 1
208  STATEMENT  bookshop  |  error 42P01: relation "reviews_archive_2019" does not exist
```

Una fila por sentencia. El método es `STATEMENT`, la ruta es el nombre de la base
de datos leído del mensaje de inicio, no hay estado HTTP y no se muestra ninguno.
El resumen lleva el SQL y cómo terminó — el command tag y su número de filas, o
el error.

**Un error de SQL es un fallo**, y el `208` de arriba lo demuestra:
`pg_errors: 1`, atrapado por `?failed=true` sin ningún código de estado a la
vista.

**Abre una sentencia y lee el intercambio:**

```bash
SQL=$(curl -s 'http://127.0.0.1:9400/api/calls?protocol=postgres&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/calls/$SQL" | jq '.postgres.sent'
```

```json
[{"type":"P","kind":"parse","size":62,"from_client":true,
  "sql":"SELECT stars, body FROM reviews WHERE sku = $1 ORDER BY id"},
 {"type":"B","kind":"bind","size":18,"from_client":true,
  "params":[{"size":4,"text":"DUNE"}]},
 {"type":"D","kind":"describe","size":2,"from_client":true},
 {"type":"E","kind":"execute","size":5,"from_client":true},
 {"type":"S","kind":"sync","size":0,"from_client":true}]
```

El SQL y el valor asociado a `$1`, juntos, en la captura en la que el texto
realmente cruzó.

### La contraseña nunca llega a la captura

Inicia sesión. El catalog abre una conexión propia para esto, así que el
handshake de inicio queda en la captura y no solo en la primera sentencia que el
proceso ejecutó alguna vez:

```bash
curl -s -X POST http://127.0.0.1:9402/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"ada@bookshop.test","password":"shelf-of-books"}'

SQL=$(curl -s 'http://127.0.0.1:9400/api/calls?protocol=postgres&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/calls/$SQL" | jq '.postgres.sent, .postgres.received[:4]'
```

**Cómo se ve un resultado correcto**

```json
[{"kind":"startup","protocol":"3.0","parameters":{"database":"bookshop","user":"shop"}},
 {"type":"p","kind":"authentication_response","size":50,
  "note":"not decoded: this message carries a password or a SASL exchange"},
 {"type":"p","kind":"authentication_response","size":104, "note":"…"},
 {"type":"Q","kind":"query","sql":"SELECT name FROM members WHERE email = 'ada@bookshop.test' AND password = 'shelf-of-books'"}]
```

```json
[{"type":"R","kind":"authentication","auth":"sasl"},
 {"type":"R","kind":"authentication","auth":"sasl_continue"},
 {"type":"R","kind":"authentication","auth":"sasl_final"},
 {"type":"R","kind":"authentication","auth":"ok"}]
```

**La contraseña de la base de datos no está y el hecho de la autenticación sí.**
El intercambio SASL se borró en la toma, mientras los bytes pasaban, antes de que
nada se guardara — reemplazado por relleno del mismo largo, así cada campo de
longitud del flujo sigue siendo cierto y la captura se sigue leyendo como una
conversación. Lo que sobrevive es que hubo una autenticación y por qué mecanismo.

Este es el único lugar donde Sonda reescribe lo que guarda, y la razón es que la
alternativa no se puede deshacer: una credencial viva en un archivo de texto
plano. **Lo que se reenvió está intacto** — la contraseña real llegó a PostgreSQL
y el login funcionó, que es por lo que hay una fila que mirar.

**Y fíjate en lo que *no* se borra aquí:** la contraseña del socio, ahí en el SQL
como literal. Eso es dato de aplicación cruzando el cable, y la API te muestra
todo, porque aquí quien lee eres tú. [El ejercicio 18](#18-las-credenciales-no-salen-por-mcp)
es esa misma sentencia pedida por MCP, donde la respuesta sale de la máquina.

**Vale la pena saberlo.** El DSN de `compose.yaml` fija
`default_query_exec_mode=exec`. Dejado en su valor por defecto, pgx cachea las
sentencias bajo un nombre y las prepara en un ciclo anterior al que las ejecuta;
Sonda solo muestra el SQL en la captura en la que el texto realmente cruzó, así
que la fila interesante diría `1 bind, 1 execute, 1 sync` sin SQL encima. Esa es
una limitación real de Sonda y esta es la perilla del lado del cliente para
sortearla.

---

## 8. Deriva de contratos

**Ejecuta**

```bash
# ya existe una referencia: la librería lleva todo este rato respondiendo /books/DUNE
curl -X POST http://127.0.0.1:8101/_control -H 'Content-Type: application/json' -d '{"drift":true}'
curl -s http://127.0.0.1:9402/books/DUNE

curl -s 'http://127.0.0.1:9400/api/drift?target=catalog&path=/books/DUNE' | jq -r .rendered
```

**Cómo se ve un resultado correcto**

```
+ cached   (boolean)
- discount_pct   (was number)
~ price_cents   number -> string
```

```bash
curl -s 'http://127.0.0.1:9400/api/drift?target=catalog&path=/books/DUNE' | jq '.baseline, .latest, .breaking'
```

```json
2
296
[{"path":"discount_pct","kind":"gone","was":"number"},
 {"path":"price_cents","kind":"retyped","was":"number","now":"string"}]
```

Tres cambios, dos de los cuales romperían a quien llama. **Agregar un campo es
seguro; perder uno o cambiarle el tipo es lo que tumba a quien llama**, y el
informe dice cuál es cuál en vez de volcar los tres como "cambios".

**Déjalo como estaba:**

```bash
curl -X POST http://127.0.0.1:8101/_control -H 'Content-Type: application/json' -d '{"drift":false}'
```

**Vale la pena saberlo.**

- Compara **formas, no valores**. Dos llamadas que devuelven precios distintos no
  son deriva. `price_cents: 1890` y `price_cents: "1890"`, sí.
- La referencia es la `2` — una de las primeras capturas que Sonda tomó de este
  endpoint. Nadie escribió un esquema y nadie mantiene ninguno; una referencia
  que hay que mantener es una referencia que en tres semanas ya no está.
- Esto es lo único en Sonda que nunca toca el proxy. Solo lee lo que ya estaba
  guardado.
- **Aquí el orden importa.** La deriva necesita que la última captura del
  endpoint sea JSON. Si acabas de correr el
  [ejercicio 12](#12-romper-cosas-a-propósito) y dejaste un 503 forzado sobre el
  catalog, la última captura es una página de fallo en texto plano y la deriva
  responde `422: call N did not answer JSON, so it has no shape to compare`.
  Limpia el fallo y haz una llamada real más.

---

## 9. Diff

### El mismo endpoint, funcionando y fallando

```bash
curl -s -X POST http://127.0.0.1:9401/checkout -H 'Content-Type: application/json' \
  -d '{"sku":"DUNE","quantity":1,"customer":"cust-1"}' > /dev/null
OK=$(curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&limit=1' | jq '.calls[0].id')

curl -s -X POST http://127.0.0.1:9401/checkout -H 'Content-Type: application/json' \
  -d '{"sku":"RESTRICTED-1","quantity":1,"customer":"cust-1"}' > /dev/null
BAD=$(curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&limit=1' | jq '.calls[0].id')

curl -s "http://127.0.0.1:9400/api/diff?a=$OK&b=$BAD" | jq '.metadata, .request.changes, [.response.changes[].path]'
```

**Cómo se ve un resultado correcto**

```json
[{"path":"http_status","kind":"changed","a":200,"b":502}]
[{"path":"sku","kind":"changed","a":"DUNE","b":"RESTRICTED-1"}]
["book","customer","detail","error","quantity","quote","sku","status","step"]
```

"Esta funcionó y esta no, ¿qué cambió?", respondido en tres líneas: el estado
pasó de 200 a 502, **exactamente un campo de la request era distinto**, y la
respuesta perdió `book`, `customer` y `quote` y ganó `error`, `step` y `detail`.

La línea del medio es la que le da sentido a la función. Dos requests que
difieren en un solo campo, y una comparación estructural que dice cuál — en vez
de un muro de rojo y verde en el que las claves reordenadas y los bloques
reindentados también parecen cambios.

### Un replay contra su original

Ver el [ejercicio 10](#10-replay).

### El diff y la deriva son preguntas distintas

Enciende el interruptor de deriva y compara dos capturas de `/books/DUNE`:

```bash
curl -s http://127.0.0.1:9402/books/DUNE > /dev/null
A=$(curl -s 'http://127.0.0.1:9400/api/calls?target=catalog&path=/books/DUNE&limit=1' | jq '.calls[0].id')
curl -X POST http://127.0.0.1:8101/_control -H 'Content-Type: application/json' -d '{"drift":true}'
curl -s http://127.0.0.1:9402/books/DUNE > /dev/null
B=$(curl -s 'http://127.0.0.1:9400/api/calls?target=catalog&path=/books/DUNE&limit=1' | jq '.calls[0].id')

curl -s "http://127.0.0.1:9400/api/diff?a=$A&b=$B" | jq '.response.changes'
curl -X POST http://127.0.0.1:8101/_control -H 'Content-Type: application/json' -d '{"drift":false}'
```

```json
[{"path":"cached","kind":"added","b":false},
 {"path":"discount_pct","kind":"removed","a":10}]
```

**Dos cambios, no tres.** La deriva informó que `price_cents` cambió de número a
texto; el diff no, porque `1890` y `"1890"` son **el mismo valor**. protojson
representa los int64 como cadena, e informar eso como una diferencia sería una
afirmación sobre la codificación y no sobre el dato.

Eso no es una inconsistencia: responden preguntas distintas. El diff pregunta
"¿es la misma respuesta?", la deriva pregunta "¿es la misma forma?". El mismo par
de capturas demuestra las dos.

**Vale la pena saberlo.** El diff es estructural, así que las claves reordenadas
y los bloques reindentados no son diferencias. El orden de un arreglo *sí* lo es
— la posición carga significado en un campo repeated de protobuf. La duración se
excluye a propósito: cambia en cada replay y enterraría las diferencias que
significan algo.

---

## 10. Replay

**Ejecuta**

```bash
curl -s -X POST http://127.0.0.1:9401/checkout -H 'Content-Type: application/json' \
  -d '{"sku":"DUNE","quantity":1,"customer":"cust-1"}' > /dev/null
ID=$(curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&limit=1' | jq '.calls[0].id')

curl -s -X POST "http://127.0.0.1:9400/api/calls/$ID/replay"
```

**Cómo se ve un resultado correcto**

```json
{"sent_to":"gateway","status":200,"duration_ms":3.64}
```

```bash
NEW=$(curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/calls/$NEW" | jq '{id, replay_of, status}'
curl -s "http://127.0.0.1:9400/api/diff?a=$ID&b=$NEW" | jq '.response.identical'
```

```json
{"id": 113, "replay_of": 104, "status": 200}
true
```

La request volvió a salir armada con los bytes que estaban guardados, así que lo
que llegó al gateway es lo que le llegó la primera vez. Se envió **a través de
Sonda**, no directo al servicio, así que el replay se captura como cualquier otro
tráfico, queda enlazado a la llamada de la que salió, y las dos se pueden
comparar de inmediato.

**Ahora intenta reenviar algo que no se puede:**

```bash
PG=$(curl -s 'http://127.0.0.1:9400/api/calls?protocol=postgres&limit=1' | jq '.calls[0].id')
curl -s -X POST "http://127.0.0.1:9400/api/calls/$PG/replay"
```

```json
{"error":"a postgres capture cannot be replayed: it belongs to a connection that
is gone, and sending it again would open a new one rather than repeat this one"}
```

**Vale la pena saberlo.** HTTP 409, y la negativa dice por qué. Un WebSocket se
rechaza por la misma razón. También una captura truncada: solo se guardó la
cabeza del cuerpo, así que lo que saldría no sería lo que se capturó, y el
resultado llevaría la palabra "replay" siendo una request distinta. Todas las
superficies se niegan, incluida la propia API, así que un agente recibe la misma
respuesta que el navegador.

El replay solo apunta a un **canal configurado** — no hay modo de URL arbitraria.
El caso útil es pedirle la misma request a una segunda instancia que ya estás
observando, y cualquier cosa más amplia convierte un depurador en una fábrica de
requests.

---

## 11. Modo stub

**Ejecuta**

```bash
curl -s -X POST http://127.0.0.1:9400/api/stub -H 'Content-Type: application/json' \
  -d '{"service":"catalog","enabled":true}'

curl -s -D - -o /dev/null http://127.0.0.1:9402/books/DUNE | grep -iE 'HTTP|x-sonda'
```

**Cómo se ve un resultado correcto**

```
HTTP/1.1 200 OK
X-Sonda-Stub: 115
```

Al catalog nunca se le llegó. Esa respuesta son los bytes de la captura `115`,
reproducidos, y la cabecera nombra cuál, así una respuesta grabada nunca se puede
confundir con una viva.

**Pídele algo de lo que no tiene grabación:**

```bash
curl -s -D - http://127.0.0.1:9402/books/NEVER-SEEN | grep -iE 'HTTP|x-sonda|Sonda is answering'
```

```
HTTP/1.1 501 Not Implemented
X-Sonda-Stub: none
Sonda is answering for "catalog" from recordings, and it has no recording of
GET /books/NEVER-SEEN. Make the call once with stubbing off, or turn it off for
this service.
```

Un 501 que se explica, no una respuesta inventada ni un 200 vacío en silencio.

**gRPC también funciona**, que es la mitad interesante — los trailers grabados se
reproducen, así el cliente recibe un `grpc-status` real en vez de esperar uno que
no llega nunca:

```bash
curl -s -X POST http://127.0.0.1:9400/api/stub -H 'Content-Type: application/json' \
  -d '{"service":"pricing","enabled":true}'
curl -s -X POST http://127.0.0.1:9401/checkout -H 'Content-Type: application/json' \
  -d '{"sku":"DUNE","quantity":1,"customer":"cust-1"}' | jq .quote
```

El checkout sigue funcionando sin que el contenedor de `pricing` se toque. Mira
la captura y lleva `stub_of`, y el árbol marca la rama con `[from recording]`.

**Déjalo como estaba:**

```bash
curl -s -X POST http://127.0.0.1:9400/api/stub -H 'Content-Type: application/json' -d '{"clear":true}'
```

**Vale la pena saberlo.** Un cuerpo de request idéntico gana de plano — esa es la
diferencia entre reproducir *la respuesta a Quote* y *la respuesta a
Quote(DUNE)*. Si no hay ninguno, la llamada más reciente al mismo método y ruta,
que para gRPC significa el mismo RPC sin importar los argumentos. Las capturas
que fueron ellas mismas un stub nunca se reutilizan: sin eso, dejar el stub
encendido iría alimentando a Sonda con sus propias respuestas. Y el stub **se
olvida al reiniciar Sonda**, a propósito — un stub que sobrevive a un reinicio es
uno que nadie recuerda haber encendido.

---

## 12. Romper cosas a propósito

### Un estado forzado, una llamada de cada tres

```bash
curl -s -X POST http://127.0.0.1:9400/api/faults -H 'Content-Type: application/json' \
  -d '{"service":"catalog","status":503,"one_in":3}'

for i in 1 2 3 4 5 6; do
  printf "call %d -> %s\n" $i "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:9402/books/DUNE)"
done
```

**Cómo se ve un resultado correcto**

```
call 1 -> 200
call 2 -> 200
call 3 -> 503
call 4 -> 200
call 5 -> 200
call 6 -> 503
```

**Determinista, no aleatorio.** Una llamada de cada tres, en ese orden, en cada
corrida. Un porcentaje se comportaría distinto cada vez y convertiría un test que
falla en una moneda al aire. Fíjate en que la que se rompe es la **tercera**
llamada, no la primera: el contador cuenta cada llamada al servicio y dispara
cuando divide. Cambiar la regla reinicia la secuencia.

### Nunca puede pasar por un fallo real

```bash
curl -s -X POST http://127.0.0.1:9400/api/faults -H 'Content-Type: application/json' \
  -d '{"service":"catalog","status":503,"latency_ms":300}'
curl -s -D - http://127.0.0.1:9402/books/DUNE | grep -iE 'HTTP|x-sonda|by Sonda'
```

```
HTTP/1.1 503 Service Unavailable
X-Sonda-Fault: answered 503 by Sonda on purpose, after 300ms of injected delay
answered 503 by Sonda on purpose, after 300ms of injected delay
```

La cabecera, el cuerpo y el `injected: true` de la captura dicen todos lo mismo.
Una regla en vigor también se dice donde se están *leyendo* los fallos — el canal
muestra `BROKEN` y la lectura de arriba en la interfaz cuenta cuántas hay
armadas —, lo que pesa sobre todo por encima de `one_in: 1`, donde las llamadas
inyectadas, por sí solas, parecen un servicio simplemente inestable.

### Una conexión cortada

```bash
curl -s -X POST http://127.0.0.1:9400/api/faults -H 'Content-Type: application/json' \
  -d '{"service":"catalog","cut":true}'
curl -s http://127.0.0.1:9402/books/DUNE          # curl exits 52, empty reply
curl -s 'http://127.0.0.1:9400/api/calls?target=catalog&limit=1' \
  | jq '.calls[0] | {id, status, injected, error}'
```

```json
{"id":154,"status":0,"injected":true,"error":"connection cut by Sonda on purpose"}
```

**Un estado o un corte terminan la llamada en Sonda** — al servicio nunca se le
llega.

### La latencia deja pasar la llamada

```bash
curl -s -X POST http://127.0.0.1:9400/api/faults -H 'Content-Type: application/json' \
  -d '{"service":"catalog","latency_ms":1500}'
curl -s -o /dev/null -w 'took %{time_total}s\n' http://127.0.0.1:9402/books/DUNE
curl -s 'http://127.0.0.1:9400/api/calls?target=catalog&limit=1' \
  | jq '.calls[0] | {status, duration_ms, injected}'
```

```json
{"status":200,"duration_ms":1502.985,"injected":null}
```

**Un fallo de solo latencia no se marca como `injected`, y vale la pena saberlo.**
Al servicio realmente se le llamó y realmente respondió 200; lo único que no es
real es cuánto tardó. No hubo ningún *fallo* inyectado que registrar — que es
exactamente el caso que un timeout debe atrapar, y el campo lo dibuja como una
marca ancha y no como una barra de fallo.

**Déjalo como estaba:**

```bash
curl -s -X POST http://127.0.0.1:9400/api/faults -H 'Content-Type: application/json' -d '{"clear_all":true}'
```

Las reglas se olvidan al reiniciar Sonda, por la misma razón que el stub.

---

## 13. Un servicio que simplemente está caído

`shipping` está declarado en la configuración de Sonda y no tiene ningún
contenedor corriendo. Ese es un fallo distinto de un servicio que respondió mal,
y debería verse distinto.

**Ejecuta**

```bash
curl -s -X POST http://127.0.0.1:9401/checkout -H 'Content-Type: application/json' \
  -d '{"sku":"DUNE","quantity":1,"customer":"cust-1","express":true}' | jq .step
curl -s 'http://127.0.0.1:9400/api/calls?target=shipping&limit=1' \
  | jq '.calls[0] | {status, error}'
```

**Cómo se ve un resultado correcto**

```json
"shipping"
{"status":502,"error":"dial tcp: lookup shipping on 127.0.0.11:53: … i/o timeout"}
```

**Un upstream inalcanzable también se captura.** Sonda responde 502 y registra el
error de transporte, así el fallo está en la línea de tiempo en vez de faltar en
ella. Un 502 que viniera del upstream *mismo* tendría el campo `error` vacío — así
distingues "el servicio dijo 502" de "el servicio no estaba".

**Ahora levántalo y no cambies nada más:**

```bash
docker compose --profile shipping up -d shipping
sleep 3
curl -s -X POST http://127.0.0.1:9401/checkout -H 'Content-Type: application/json' \
  -d '{"sku":"DUNE","quantity":1,"customer":"cust-1","express":true}' > /dev/null
ID=$(curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/trace?call=$ID" | jq -r .rendered
```

```
gateway /checkout                                              7ms ✓
├─ catalog /books/DUNE                                         1ms ✓
│  └─ catalog-db bookshop                                      0ms ✓
├─ pricing /shop.v1.Pricing/Quote                              1ms ✓
├─ storefront /graphql                                         1ms ✓
└─ shipping /quote                                             1ms ✓
```

La misma petición, seis llamadas, todas verdes, y los 1005 ms pasaron a 4 ms.
Déjalo como estaba con `docker compose stop shipping`.

**Vale la pena saberlo.** Un contenedor detrás de un profile no lo elimina un
`docker compose down` a secas, así que si bajas el stack y lo vuelves a levantar,
el contenedor viejo de `shipping` sigue ahí con una referencia a una red que ya
no existe, y arrancarlo falla con `network … not found`. `docker compose rm -sf
shipping` lo limpia, y `docker compose down -v --profile shipping` lo evita desde
el principio.

---

## 14. WebSocket y server-sent events

### Un socket que cierra mal

El driver sostiene dos conversaciones en cada ciclo: una que termina limpia y una
que le dice `boom` al storefront.

```bash
docker compose start driver && sleep 25 && docker compose stop driver

curl -s 'http://127.0.0.1:9400/api/calls?protocol=websocket&limit=2' \
  | jq -r '.calls[] | "\(.id)  status=\(.status)  \(.duration_ms)ms"'

WS=$(curl -s 'http://127.0.0.1:9400/api/calls?protocol=websocket&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/calls/$WS" | jq .socket
```

**Cómo se ve un resultado correcto**

```json
{"sent":[{"kind":"text","final":true,"size":4,"text":"boom"}],
 "received":[
   {"kind":"text","final":true,"size":42,"text":"{\"event\":\"welcome\",\"shelf\":\"new arrivals\"}"},
   {"kind":"close","final":true,"size":36,
    "close_code":1011,"close_reason":"inventory feed lost, no shelf data"}],
 "sent_summary":"1 text","received_summary":"1 text, 1 close",
 "sent_incomplete":false,"received_incomplete":false}
```

**El frame de cierre informa su código y su motivo, y esa suele ser la respuesta
a por qué el socket dejó de funcionar.** Compáralo con la otra captura, que
cierra con 1000.

Fíjate en que el frame del cliente se lee como `boom` en texto plano: los frames
del cliente se muestran **sin la máscara**, porque la máscara es andamiaje del
transporte que el extremo receptor quita antes de que nada lea el payload. La
clave se conserva para que el frame se pueda reproducir exacto.

**Vale la pena saberlo.** El estado es `101` —el handshake funcionó— y **la
captura solo aparece cuando el socket cierra**, no mientras está abierto. Un
socket es un intercambio y el intercambio no ha terminado. El handshake mismo
pasa textual: la clave, los subprotocolos y las extensiones negociadas son lo que
los dos extremos están acordando, y cambiar cualquiera de ellos volvería la
conversación grabada una conversación distinta.

**Y la parte que más vale la pena saber:** ninguna de esas dos capturas se cuenta
como fallo.

```bash
curl -s 'http://127.0.0.1:9400/api/calls?protocol=websocket&failed=true&limit=5' | jq '.calls | length'
curl -s 'http://127.0.0.1:9400/api/calls?protocol=websocket&limit=2' \
  | jq -r '.calls[] | "\(.id)  status=\(.status)  error=\(.error)"'
```

```
0
87  status=101  error=null
86  status=101  error=null
```

El socket que murió con `1011 inventory feed lost` y el socket que se despidió
son, para el filtro de fallos, la misma fila. Sonda promueve a fallo un estado de
gRPC, un arreglo `errors` de GraphQL y un `ErrorResponse` de Postgres —tres
protocolos que reportan problemas por debajo del transporte— y un código de
cierre de WebSocket es un cuarto caso exactamente de esa forma que no promueve.
La información está toda ahí, en `socket.received[].close_code`; nada por encima
de 1000 llega al filtro de fallos, a los conteos del riel de canales ni a
`recent_failures`.

Así que este es el único fallo del banco de pruebas que tienes que ir a buscar:

```bash
curl -s 'http://127.0.0.1:9400/api/calls?protocol=websocket&limit=10' \
  | jq -r '.calls[].id' \
  | while read id; do
      curl -s "http://127.0.0.1:9400/api/calls/$id" \
        | jq -r --arg id "$id" '.socket.received[] | select(.close_code) | "\($id)  \(.close_code)  \(.close_reason)"'
    done
```

```
87  1011  inventory feed lost, no shelf data
86  1000  goodbye
```

### Un flujo de eventos

```bash
curl -s http://127.0.0.1:9403/events > /dev/null
SSE=$(curl -s 'http://127.0.0.1:9400/api/calls?path=/events&limit=1' | jq '.calls[0].id')
curl -s "http://127.0.0.1:9400/api/calls/$SSE" | jq '{protocol, status, stream}'
```

```json
{"protocol":"http","status":200,
 "stream":{"events":[
   {"name":"shelf","id":"1","data":"{\"sku\":\"DUNE\",\"on_hand\":4}"},
   {"name":"shelf","id":"2","data":"{\"sku\":\"DUNE\",\"on_hand\":3}"},
   {"name":"shelf","id":"3","data":"{\"sku\":\"DUNE\",\"on_hand\":2}"},
   {"name":"done","data":"{\"sent\":3}"}],
  "incomplete":false}}
```

**Vale la pena saberlo.** Un flujo de eventos **no es un protocolo aparte** — la
respuesta es HTTP corriente, `protocol: "http"`, y Sonda lo reconoce por el
content type de la respuesta y lo vuelve a separar en sus eventos para mostrarlo.
El storefront manda primero un comentario `: keep-alive` y **no** está en la
lista: los comentarios y los keepalives se descartan en vez de mostrarse como
eventos. `?broken=1` termina el flujo con un evento `error` en vez de `done`.

---

## 15. TLS

`storefront-tls` es el mismo storefront detrás de un listener que responde con un
certificado propio, para quien llama y se niega a hablar `http://`.

**Ejecuta**

```bash
curl -s http://127.0.0.1:9400/api/tls | jq '{exists, subject: .instructions.subject, path: .instructions.path}'
curl -s http://127.0.0.1:9400/api/tls/ca.pem -o sonda-ca.pem
```

```json
{"exists": true,
 "subject": "Sonda local CA (6bc4a74c91a4)",
 "path": "/data/sonda-ca.pem"}
```

La autoridad se creó la primera vez que arrancó un servicio con `tls: true` — no
en la primera ejecución. Una Sonda sin ningún target TLS nunca crea una y no
imprime nada. El `path` está dentro del contenedor, y por eso hay un endpoint de
descarga.

**Ahora llámalo:**

```bash
# falla: nada en esta máquina confía en esa autoridad, y Sonda no instala nada
curl -s https://localhost:9407/health

# funciona
curl -s --cacert sonda-ca.pem https://localhost:9407/health
```

```json
{"status":"ok"}
```

```bash
curl -s --cacert sonda-ca.pem -X POST https://localhost:9407/graphql \
  -H 'Content-Type: application/json' -d '{"query":"query Shelf { shelf }"}'
curl -s 'http://127.0.0.1:9400/api/calls?target=storefront-tls&limit=1' \
  | jq '.calls[0] | {id, status, tls, graphql_op}'
```

```json
{"id":158,"status":200,"tls":true,"graphql_op":"query Shelf"}
```

`tls: true` en la captura: una respuesta leída meses después sigue diciendo si la
conexión hacia Sonda estaba cifrada. El salto hacia el upstream es HTTP plano,
así que no hay `upstream_tls` — las dos mitades se registran por separado, porque
son dos hechos distintos.

**Dos cosas que te van a morder, y las dos son cosa del cliente y no de Sonda:**

- **Usa `localhost`, no `127.0.0.1`.** Un cliente que se conecta a una IP literal
  no envía SNI, así que Sonda emite el certificado para la dirección por la que
  llegó la conexión — que, dentro de un contenedor, es la dirección propia del
  contenedor en la red de compose, no `127.0.0.1`. La verificación falla entonces
  con *"certificate is not valid for 127.0.0.1"*. Un hostname sí envía SNI y el
  certificado se emite para el nombre que se pidió.
- **En Windows**, `curl.exe` usa schannel, que no puede comprobar la revocación
  de una autoridad privada y falla con
  `CERT_TRUST_REVOCATION_STATUS_UNKNOWN` incluso cuando el `--cacert` está bien.
  Agrega `--ssl-no-revoke`. macOS y Linux no necesitan nada extra.

**Vale la pena saberlo.** Sonda **no instala nada**. Escribe los dos archivos
junto a la base de datos, imprime qué ejecutar, y para ahí; `curl -s
http://127.0.0.1:9400/api/tls | jq .instructions` tiene la línea exacta para cada
plataforma y la línea exacta para deshacerlo. Modificar el almacén de confianza
de una máquina es una decisión con una mano humana encima — una herramienta de
depuración que lo hiciera calladamente sería indistinguible de malware.

---

## 16. ¿Por qué no veo nada?

El informe que responde la primera hora más común con una herramienta así.

**Ejecuta**

```bash
# solo lee lo que Sonda ya sabe, no toca la red
curl -s http://127.0.0.1:9400/api/diagnose | jq '{verdict, summary}'

# además marca una vez cada upstream y corta
curl -s -X POST http://127.0.0.1:9400/api/diagnose \
  | jq -r '.services[] | "\(.service)  \(.verdict)  connections=\(.connections) captures=\(.captures) reachable=\(.upstream_reachable)"'
```

**Cómo se ve un resultado correcto**

```
gateway         capturing  connections=7  captures=24  reachable=true
catalog         capturing  connections=30 captures=56  reachable=true
storefront      capturing  connections=6  captures=21  reachable=true
pricing         capturing  connections=3  captures=18  reachable=true
shipping        capturing  connections=2  captures=2   reachable=null
catalog-db      capturing  connections=3  captures=36  reachable=true
storefront-tls  capturing  connections=9  captures=2   reachable=true
```

**Vale la pena saberlo.**

- **Sondear es un efecto secundario**, y por eso está en el `POST` y no en el
  `GET`. Averiguar si un servicio está arriba significa marcarlo, y eso es
  tráfico que el usuario no envió. Nunca ocurre al cargar la página, al refrescar
  ni por un temporizador. La conexión va directo al servicio, nunca por el
  listener de Sonda, así que jamás puede aparecer en la lista de capturas como si
  fuera una llamada tuya.
- **`connections` es la lectura que más trabajo hace.** Cuenta cada conexión TCP
  que el puerto aceptó, se haya convertido en llamada o no. Mira
  `storefront-tls`: si corriste el [ejercicio 15](#15-tls) e hiciste la llamada
  una vez antes de bajar la CA, su cuenta de conexiones es más alta que su cuenta
  de capturas, y la diferencia son exactamente los handshakes que fallaron.
  Conexiones sin capturas es un cliente que encontró a Sonda y fue
  malinterpretado — TLS contra un listener en claro o al revés, o un protocolo
  que Sonda no proxea. Cero conexiones es un cliente que nunca llegó. Son
  problemas distintos con soluciones distintas, y sin ese contador se leen
  exactamente igual.
- **Sonda no puede ver un cliente que nunca se conectó a ella.** Un puerto sin
  conexiones se lee igual si quien llama sigue hablando directo con el servicio,
  si está apuntado a otro puerto, o si todavía no se ejecutó. El informe nombra
  las tres en vez de elegir una.
- **Un veredicto es lo peor que es cierto, y `capturing` gana por sobre un sondeo
  fallido.** Detén un servicio y sondea:

  ```bash
  docker compose stop catalog
  curl -s -X POST http://127.0.0.1:9400/api/diagnose \
    | jq -r '.services[] | select(.service=="catalog") | .verdict, .detail'
  docker compose start catalog
  ```

  ```
  capturing
  73 call(s) captured here, 18 flagged, the newest 19s ago. Traffic is reaching
  Sonda. The upstream http://catalog:8101 also refused a connection (dial tcp:
  lookup catalog …).
  ```

  Sigue siendo `capturing`, y es la respuesta correcta: a este puerto realmente
  está llegando tráfico, y el proxy realmente lo está registrando. El upstream
  muerto va en el detalle y no en el veredicto. `upstream_unreachable` es lo que
  recibe un servicio **sin** capturas — un puerto por el que nunca pasó nada, que
  es el caso donde el sondeo fallido es toda la historia.

---

## 17. La superficie MCP

Sonda habla el Model Context Protocol, así que un agente de código lee las
capturas por su cuenta en vez de que se las cuenten. Es un POST JSON-RPC pelado
—**sin handshake, sin id de sesión, sin SSE**— así que todos estos se pueden
pegar tal cual.

**Lista las herramientas**

```bash
curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | jq -r '.result.tools[].name'
```

**Cómo se ve un resultado correcto** — veinte de ellas:

```
recent_failures  search_calls  get_call  trace_call  contract_drift  diff_calls
list_services  schema_status  diagnose_silence  trust_certificate  wait_for_call
replay_call  connect_project  configure_service  remove_service  upload_schemas
activate_project  set_stub  break_service  disconnect_project
```

**"¿Qué se rompió recién?"**

```bash
curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call",
       "params":{"name":"recent_failures","arguments":{"limit":5}}}' \
  | jq -r '.result.content[0].text'
```

**La petición completa a la que perteneció una llamada**

```bash
ID=$(curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&limit=1' | jq '.calls[0].id')
curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",
       \"params\":{\"name\":\"trace_call\",\"arguments\":{\"id\":$ID}}}" \
  | jq -r '.result.content[0].text | fromjson | .rendered'
```

```
(grouped by timing, not by a trace id — the shape is inferred)
gateway /checkout                                              3ms ✓
├─ catalog /books/SOLARIS                                      1ms ✓
│  └─ catalog-db bookshop                                      0ms ✓
├─ pricing /shop.v1.Pricing/Quote                              0ms ✓  [from recording]
└─ storefront /graphql                                         0ms ✓
```

**Qué se está observando, y qué está en stub o roto en este momento**

```bash
curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":4,"method":"tools/call",
       "params":{"name":"list_services","arguments":{}}}' \
  | jq -r '.result.content[0].text'
```

Cada servicio vuelve con la línea exacta para apuntarle a quien llama —
`CATALOG_URL=0.0.0.0:9402` — porque **Sonda no puede reapuntar a quien llama**.
Es un proxy explícito y no ve nada hasta que a alguien le dicen que le hable.
Sonda sabe el mapeo; el agente tiene el sistema de archivos y puede reiniciar un
proceso.

La respuesta lleva además qué está en stub y qué se está rompiendo **en este
momento**, que es lo que un agente que lee capturas necesita antes de sacar
cualquier conclusión de ellas. Arma las dos cosas y vuelve a preguntar:

```json
{"stubbed": ["pricing"],
 "faults": {"catalog": "HTTP 503, one call in 10"},
 "projects": [ … ]}
```

**De dónde salieron los nombres de campo de gRPC**

```bash
curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":5,"method":"tools/call",
       "params":{"name":"schema_status","arguments":{}}}' \
  | jq -r '.result.content[0].text'
```

```json
{"schemas":[{"target":"pricing","source":"descriptor_set","reflection":true}]}
```

**`source: descriptor_set` es el punto de este ejercicio.** El servicio `pricing`
no sirve reflection —la mayoría de los servicios de la vida real tampoco— así que
Sonda no pudo preguntarle cuál era su esquema. En vez de eso leyó el descriptor
set compilado que está montado en `/etc/bookshop/descriptors.binpb`, que es por
lo que `{"sku":"RESTRICTED-1","quantity":1}` vuelve con nombres de campo en vez
de como `{"number":1,"type":"string","value":"RESTRICTED-1"}`.

Para ver cómo se ve el tercer respaldo, borra la línea `descriptor_set:` de
`sonda.testbed.yaml`, corre `docker compose down -v && docker compose up -d`,
espera un ciclo, y lee una captura de `Quote`:

```json
{"schemas":[{"target":"pricing","source":"","reflection":true,
             "error":"no schema source configured"}]}
```

```json
{"index":0,"size":16,
 "fields":[{"number":1,"type":"string","value":"SOLARIS"},
           {"number":2,"type":"varint","value":2,
            "note":"could be an integer, a bool or an enum"},
           {"number":3,"type":"string","value":"USD"}]}
```

El mensaje igual se decodifica —la estructura anidada intacta, los tipos leídos
del cable— y **las adivinanzas están etiquetadas como adivinanzas**. En el cable
un varint realmente podría ser un int32, un bool o un enum, y decirlo es la
diferencia entre una vista útil y una engañosa. Devuelve la línea a su lugar y
`down -v && up -d` de nuevo.

*(El `reflection: true` de esa respuesta es un bug de reporte en Sonda — la
bandera no se relee del servicio guardado. `source` es la respuesta real.)*

**Esperar tráfico que todavía no ocurrió**

Esta es la herramienta que convierte a Sonda en un verificador y no en un visor:
el agente hace un cambio, dispara la acción, y espera lo que debería haber
cruzado el cable.

```bash
# en una terminal
curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":6,"method":"tools/call",
       "params":{"name":"wait_for_call",
                 "arguments":{"service":"catalog","path":"/books/SOLARIS","timeout_seconds":15}}}' \
  | jq -r '.result.content[0].text | fromjson | {matched, waited_seconds}'

# en otra, un par de segundos después
curl -s http://127.0.0.1:9402/books/SOLARIS > /dev/null
```

```json
{"matched": true, "waited_seconds": 2}
```

Que no llegue nada también es una respuesta: déjalo expirar y vuelve
`{"matched": false, "hint": "…run diagnose_silence…"}`.

**Romper un servicio pidiéndolo**

```bash
curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":7,"method":"tools/call",
       "params":{"name":"break_service","arguments":{"service":"storefront","latency_ms":1200,"one_in":2}}}' \
  | jq -r '.result.content[0].text'

for i in 1 2; do
  curl -s -o /dev/null -w "call $i took %{time_total}s\n" -X POST http://127.0.0.1:9403/graphql \
    -H 'Content-Type: application/json' -d '{"query":"query Shelf { shelf }"}'
done

curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":8,"method":"tools/call",
       "params":{"name":"break_service","arguments":{"clear_all":true}}}' > /dev/null
```

```
call 1 took 0.003453s
call 2 took 1.218154s
```

**Vale la pena saberlo.** `set_stub`, `break_service`, `replay_call`,
`activate_project` y `disconnect_project` están marcadas como destructivas, así
que un cliente MCP de verdad le pregunta al humano antes de ejecutarlas. El
JSON-RPC pelado no pregunta, porque no hay a quién preguntarle.

También a propósito: **no hay ninguna herramienta para borrar un proyecto**, no
hay stream en vivo, no hay forma de descargar la clave privada de la autoridad
certificadora, y **no hay ningún ajuste que apague la redacción** — que es el
ejercicio siguiente.

---

## 18. Las credenciales no salen por MCP

La interfaz web muestra los payloads de aplicación almacenados que se usan aquí,
porque allí quien lee es el usuario. Las respuestas de MCP salen de la máquina,
así que se filtran. Los secretos del handshake de Postgres y AMQP no aparecen en
ninguna de las dos porque Sonda los borra antes de persistirlos. El banco de
pruebas pone una credencial en cada uno de los tres lugares a los que debe llegar
el filtrado de MCP.

**Prepáralo**

```bash
curl -s -X POST http://127.0.0.1:9402/login \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer tok-live-2f8c11' \
  -d '{"email":"ada@bookshop.test","password":"shelf-of-books"}' > /dev/null

curl -s "http://127.0.0.1:9401/oauth/callback?code=ac-9f21&access_token=tok-live-2f8c11&state=shelf" > /dev/null

LOGIN=$(curl -s 'http://127.0.0.1:9400/api/calls?target=catalog&path=/login&limit=1' | jq '.calls[0].id')
SQL=$(curl -s 'http://127.0.0.1:9400/api/calls?protocol=postgres&limit=1' | jq '.calls[0].id')
```

### Una cabecera y un cuerpo JSON

```bash
# la API: todo
curl -s "http://127.0.0.1:9400/api/calls/$LOGIN" | jq '{auth: .request.headers.Authorization, body: .request.text}'

# MCP: la misma llamada
curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":9,\"method\":\"tools/call\",
       \"params\":{\"name\":\"get_call\",\"arguments\":{\"id\":$LOGIN,\"detail\":true}}}" \
  | jq -r '.result.content[0].text | fromjson | {auth: .request.headers.Authorization, body: .request.text}'
```

```json
{"auth": ["Bearer tok-live-2f8c11"],
 "body": "{\"email\":\"ada@bookshop.test\",\"password\":\"shelf-of-books\"}"}
```

```json
{"auth": ["[redacted by Sonda]"],
 "body": "{\"email\":\"ada@bookshop.test\",\"password\":\"[redacted by Sonda]\"}"}
```

La cabecera se fue por nombre; la clave del cuerpo se fue por nombre, **dentro de
un cuerpo JSON que Sonda está guardando como bytes opacos**. El email se queda,
porque no es una credencial.

### Una query string

```bash
curl -s 'http://127.0.0.1:9400/api/calls?target=gateway&path=/oauth/callback&limit=1' | jq -r '.calls[0].path'

curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":10,"method":"tools/call",
       "params":{"name":"search_calls","arguments":{"path":"/oauth/callback","limit":1}}}' \
  | jq -r '.result.content[0].text | fromjson | .calls[0].path'
```

```
/oauth/callback?code=ac-9f21&access_token=tok-live-2f8c11&state=shelf
/oauth/callback?code=[redacted by Sonda]&access_token=[redacted by Sonda]&state=shelf
```

Calzar un nombre de campo solo funciona sobre un campo que tenga uno, y una URL
no tiene campos. Así que las query strings reciben su propia pasada, dondequiera
que aparezca una URL — la ruta capturada, un redirect `Location`, un enlace
dentro de un cuerpo. **El resto de la URL se conserva**, incluido `state`, porque
la ruta es como reconoces la llamada.

### Una sentencia SQL

Esta es la más difícil, y la razón es que Postgres es orientado a columnas: el
nombre sensible y el valor sensible llegan en mensajes distintos.

```bash
curl -s "http://127.0.0.1:9400/api/calls/$SQL" | jq -r .postgres_summary

curl -s http://127.0.0.1:9400/mcp -H 'Content-Type: application/json' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":11,\"method\":\"tools/call\",
       \"params\":{\"name\":\"get_call\",\"arguments\":{\"id\":$SQL,\"detail\":true}}}" \
  | jq -r '.result.content[0].text | fromjson | .postgres_summary'
```

```
SELECT name FROM members WHERE email = 'ada@bookshop.test' AND password = 'shelf-of-books' -> SELECT 1
SELECT name FROM members WHERE email = '[redacted by Sonda]' AND password = '[redacted by Sonda]' -> SELECT 1
```

**Una sentencia que nombra una credencial vuelve con su estructura intacta y sus
literales borrados.** Todos, incluido el email — se borra de más a propósito,
porque saber qué literal corresponde a qué columna es trabajo de un parser de SQL
y equivocarse aquí no se puede deshacer. Igual puedes ver qué hizo la consulta,
que es a lo que veniste.

Y esto pasa en el **resumen de una línea que muestra un listado antes de que
hayas pedido nada**, no solo en el detalle. Una credencial que solo se redacta
cuando abres la llamada es una credencial que ya salió en los resultados de
búsqueda.

**Vale la pena saberlo.** No hay ningún ajuste para apagar nada de esto,
deliberadamente: una bandera para eso se encendería contra un proyecto de juguete
y después quedaría olvidada contra uno real. `SECURITY.md` lista hasta dónde
llega la redacción y, más útil todavía, hasta dónde no — un campo protobuf
decodificado sin esquema tiene un número y no un nombre, así que no hay nada que
calzar por nombre.

---

## 19. El cliente de terminal

El mismo instrumento en una terminal, y un segundo cliente de la misma API en vez
de una segunda implementación: no captura nada y no guarda nada.

```bash
docker compose run --rm tui
```

**Cómo se ve un resultado correcto.** El riel de canales con los seis canales de
la librería, sus conteos de llamadas y sus conteos de fallos; el campo, con un
**bloque completo donde una llamada normal es medio bloque** — un carril mide una
fila, así que un fallo no puede ser una barra más alta y pasa a ser otro glifo.
La forma sigue cargando el resultado antes que el color.

Prueba, en orden:

| Tecla | Qué mirar |
|---|---|
| `↑` `↓` | elegir el canal `gateway` |
| `←` `→` | recorrerlo, llamada por llamada — no celda por celda, una celda vacía no es algo a lo que apuntar |
| `enter` | leer la llamada seleccionada |
| `t` | la petición completa como árbol, el mismo que produjo el ejercicio 5 |
| `c` | la deriva de contrato de este endpoint |
| `d` | comparar un replay contra su original |
| `f` | solo fallos, o todo |
| `/` | buscar |
| `q` | salir |

Arma un fallo (`break_service` o `POST /api/faults`) y mira el canal: el bloque
de fallo queda grabado delante de su nombre y la barra cuenta cuántos hay
armados. Un servicio roto a propósito es un **modo** y no una llamada, así que
pertenece al canal y no al campo.

---

## Detenerlo

```bash
docker compose --profile shipping --profile tui down      # keeps the captures
docker compose --profile shipping --profile tui down -v   # throws the database away too
```

Nombra los profiles. Un `docker compose down` a secas deja en pie cualquier
contenedor que hayas levantado detrás de un profile, apuntando a una red que
acaba de borrarse, y el siguiente `up` para él falla con `network … not found`.

El volumen vale la pena conservarlo entre sesiones: la deriva de contratos compara
contra la captura más antigua que Sonda tiene, y arrancar limpio tira esa
referencia a la basura.

## Qué no cubre este banco de pruebas

Se dice porque un recorrido que se salta algo calladamente es peor que uno que
dice qué se saltó.

- **AMQP.** Sonda captura AMQP 0-9-1, pero este banco de pruebas no incluye un
  broker RabbitMQ ni un ejercicio AMQP. Las pruebas enfocadas del proxy usan un
  broker determinista a nivel de protocolo, sin presentarlo como prueba de
  compatibilidad con RabbitMQ.
- **Kafka.** Deliberadamente ausente de Sonda; el README del repositorio explica
  por qué, y no es por el protocolo.
- **Un upstream que habla TLS.** `storefront-tls` ejercita la mitad donde el
  *cliente* habla TLS. La otra mitad —Sonda verificando un certificado real de
  salida, y `insecure_skip_verify` para uno autofirmado— necesita una autoridad
  certificadora en la que todo el sistema esté de acuerdo, que es un aparato más
  grande del que un banco de pruebas debería cargar. La configuración son dos
  líneas en `sonda.example.yaml` si quieres apuntar uno a una API HTTPS real.
- **gRPC con streaming del cliente.** Unario y streaming del servidor están aquí;
  la tercera forma está en `examples/grpcdemo`, en el repositorio principal.
- **Truncado y topes de cuerpo.** `max_body_bytes` está en su valor por defecto y
  nada de aquí manda un payload cercano. Bájalo a `512` en `sonda.testbed.yaml` y
  cada captura empieza a informar `truncated`, y el replay empieza a negarse.
- **Un mensaje gRPC comprimido.** Se informa como comprimido y no se decodifica;
  nada de aquí negocia compresión.
- **Cambiar de proyecto e importar.** Un solo proyecto, sembrado desde
  `sonda.testbed.yaml`. `POST /api/discover` contra el `compose.yaml` de este
  directorio es algo razonable de probar, y la pantalla `PROJECTS` es donde vive
  el resto.

---

## Cómo está construido

`testbed/` es **su propio módulo de Go**. Sonda es un único binario estático sin
cgo, que es por lo que seis plataformas se compilan cruzado desde un solo runner
y por lo que la imagen pesa 50 MB; un cliente de PostgreSQL en el `go.mod`
principal terminaría con eso. Nada de aquí cambia el `go.mod` ni el `go.sum` del
módulo principal, y `go build ./...` en la raíz del repositorio no baja hasta
aquí.

```
testbed/
  compose.yaml            la librería, y una Sonda que la observa
  sonda.testbed.yaml      los siete targets de Sonda
  descriptors.binpb       el esquema de pricing, para un servicio sin reflection
  db/init.sql             cinco libros, cuatro reseñas, dos socios
  cmd/gateway             el fan-out
  cmd/catalog             HTTP + PostgreSQL
  cmd/storefront          GraphQL + WebSocket + SSE
  cmd/pricing             gRPC, sin reflection
  cmd/shipping            apagado, a propósito
  cmd/driver              dieciocho pasos, cada veinte segundos
  internal/toy            respuestas JSON, los interruptores, propagación del trace id
  internal/ws             un WebSocket lo bastante chico para leerlo
```

Después de cambiar `proto/shop/v1/pricing.proto`:

```bash
cd testbed
buf lint && buf generate && buf build -o descriptors.binpb
```
