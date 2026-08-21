[← Docs](README.md)

# Protocolos

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

# and the contrast: only the ones that did not. Leave failed out for both
curl 'http://127.0.0.1:9000/api/calls?failed=false'
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

Cada una de esas líneas nombra una ruta, y cuando Sonda corre en un contenedor
la ruta que imprime es una ruta dentro del contenedor: en tu máquina no existe
ningún `/data`. Primero hay que traer el archivo al disco propio y usar esa
ruta:

```bash
docker compose cp sonda:/data/sonda-ca.pem ./sonda-ca.pem
# o, sin Docker de por medio
curl -o sonda-ca.pem http://127.0.0.1:9000/api/tls/ca.pem
```

Un agente no tiene ninguna de las dos: `trust_certificate` devuelve el
certificado mismo en `certificate_pem`, así que puede escribir el archivo y
entregar la ruta donde lo escribió.

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

## AMQP 0-9-1

Declara RabbitMQ como un target `amqp` y apunta el cliente a Sonda, conservando
el usuario, la contraseña y el host virtual del propio cliente:

```yaml
  - name: events
    listen: 127.0.0.1:9401
    upstream: amqp://127.0.0.1:5672
    protocol: amqp
```

```text
AMQP_URL=amqp://app:change-me@127.0.0.1:9401/orders
```

El upstream configurado no debe contener credenciales. Sonda reenvía byte a
byte el handshake del cliente y no necesita una segunda copia de la contraseña
en la configuración.

Usa `amqps://` cuando el salto hacia el broker use TLS. El TLS del listener es
independiente:

```yaml
  - name: secure-events
    listen: 127.0.0.1:9402
    upstream: amqps://rabbitmq.internal:5671
    protocol: amqp
    tls: true
```

Aquí Sonda verifica el certificado del broker y el llamador usa
`amqps://127.0.0.1:9402`. Igual que con HTTPS, el llamador debe confiar en la
autoridad local de Sonda. `insecure_skip_verify: true` solo se acepta para este
target `amqps://`, queda registrado en todas las superficies y no es un
interruptor global.

### Qué significa una captura

AMQP es una sesión bidireccional y no un protocolo de petición y respuesta, por
lo que Sonda guarda unidades útiles por dirección en vez de inventar pares:

- `basic.publish`, `basic.return`, `basic.deliver` y `basic.get-ok` incluyen la
  cabecera de contenido y las tramas de cuerpo de ese canal.
- Los métodos de handshake, topología, confirmación y cierre quedan como
  unidades independientes. Las tramas de heartbeat se reenvían, pero no se
  almacenan.
- El listado muestra el método AMQP y una ruta, cola, host virtual o canal que se
  puede buscar. El texto del mensaje y las respuestas del broker también se
  indexan.
- Los errores `close` y `return` del broker con códigos de respuesta desde 300
  aparecen como fallos en la API, el campo web, la terminal y
  `recent_failures`.

`GET /api/calls/{id}` entrega las tramas decodificadas bajo `amqp.sent` y
`amqp.received`, además de `sent_incomplete` y `received_incomplete`. El
inspector web y la TUI muestran esas mismas tramas. `get_call` por MCP devuelve
la vista decodificada y elimina la copia sin procesar redundante;
`search_calls` acepta `protocol: "amqp"` y los mismos filtros de método, ruta y
texto que las demás capturas.

### Credenciales y límites

Lo que recibe el broker permanece intacto. En la copia almacenada por Sonda, los
desafíos y respuestas SASL de `connection.start-ok`, `connection.secure` y
`connection.secure-ok` se reemplazan por ceros, mientras se conserva el nombre
del mecanismo seleccionado. Si la conexión termina a mitad de una trama que
podría contener una credencial, Sonda registra el tamaño y el error, pero no
guarda ninguno de esos bytes parciales.

Una trama que supera el límite de 16 MiB del analizador de captura igual se
reenvía, pero solo se guarda su tamaño y el error. `max_body_bytes` limita por
separado cuántos bytes de una unidad válida llegan a SQLite; el reenvío sigue
siendo completo. Estas capturas no se pueden reenviar, usar como stub ni someter
a inyección de fallos: hacerlo exigiría recrear una conexión y un estado de canal
que ya no existen. Sonda admite AMQP **0-9-1**, no AMQP 1.0, y no reconstruye
transacciones del broker ni empareja publicaciones con entregas.

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

