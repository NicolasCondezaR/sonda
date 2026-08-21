[← Docs](README.md)

# Agentes

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
| `search_calls` | Por servicio, método, ruta, estado o texto en los cuerpos. `failed` tiene tres estados: true para las fallidas, false para las que funcionaron, ausente para ambas |
| `get_call` | Una llamada completa, decodificada |
| `diff_calls` | "Esta funcionó y esta no, ¿qué cambió?" |
| `trace_call` | Todas las llamadas que fueron parte de la misma petición, como árbol |
| `list_services` | Qué se está observando, en qué puertos, si está escuchando — y qué está respondiendo desde grabaciones o roto a propósito en este momento |
| `schema_status` | De dónde salieron los nombres de campo de cada servicio gRPC: reflection, el descriptor set, o nada. Cuando un servicio no resolvió nada, también nombra los servicios afectados y el comando que compila un descriptor set para ellos |
| `wait_for_call` | Bloquea hasta que aparezca tráfico que calce. Dispara algo y verifica. `failed` tiene los mismos tres estados |
| `replay_call` | Reenvía una captura. Marcada como destructiva, el cliente pregunta antes |
| `connect_project` | Configura Sonda para observar un sistema entero, y devuelve la edición que hace pasar el tráfico por ella. Se puede volver a ejecutar |
| `configure_service` | Agrega un servicio, o cambia uno que ya está — el nombre es la identidad, así que llamarla de nuevo mueve el puerto. Una modificación conserva todo lo que no se le pasó |
| `remove_service` | Borra un servicio y dice a qué dirección volver a apuntar a quien llamaba. Pregunta antes |
| `upload_schemas` | Entrega al proyecto un descriptor set compilado, para decodificar gRPC donde ningún servicio ofrece reflection. El transporte stdio local acepta una `path`; MCP por HTTP acepta contenido en base64 |
| `activate_project` | Abre los puertos. Pregunta antes |
| `disconnect_project` | Los cierra y devuelve la edición que deshace el apuntado. Pregunta antes |
| `set_stub` | Responder por un servicio desde grabaciones en vez de reenviar. Pregunta antes |
| `break_service` | Agregar latencia, forzar un estado o cortar la conexión. Pregunta antes |
| `contract_drift` | Si esta respuesta cambió de forma desde que funcionaba |
| `trust_certificate` | Los bytes de la autoridad certificadora de Sonda, dónde la guarda y qué ejecutar para confiar en ella o quitarla |
| `diagnose_silence` | «¿Por qué no veo nada?»: por servicio, si el puerto se abrió, si algo se conectó, qué se capturó y qué causas no se pueden distinguir |

`wait_for_call` es la que convierte a Sonda en un verificador y no solo en un
visor: el agente hace un cambio, dispara la acción y espera lo que debería haber
cruzado. Que no llegue nada también es una respuesta.

`upload_schemas` tiene dos transportes con acceso a archivos deliberadamente
distinto:

```json
{ "project": "core-delpagroup", "path": "C:\\work\\descriptors.binpb" }
```

La forma con `path` solo está disponible para el proceso stdio local
`sonda mcp`. Acepta un archivo de hasta 32 MiB en la máquina que lo inició,
conserva el nombre base de la ruta como nombre del descriptor y envía los bytes
a Sonda. El endpoint MCP por HTTP en `/mcp` nunca lee una ruta del sistema de archivos de
Sonda. Allí se usa la forma existente por contenido, que también funciona por
stdio si el JSON cabe en el límite de mensaje:

```json
{ "project": "core-delpagroup", "filename": "descriptors.binpb", "content_base64": "..." }
```

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
Esa dirección se puede usar directamente. El campo `point_at` de la API de
proyectos y `listening_on` de `configure_service` devuelven `http://host:port`
para HTTP —o `https://` con TLS en el listener—, mientras que AMQP devuelve
`amqp://host:port` o `amqps://host:port`. gRPC en texto plano conserva
`host:port` —con TLS en el listener pasa a `https://host:port`—, y Postgres
conserva `host:port` para insertarlo en el DSN del propio llamador.

### Qué le cuesta una respuesta a un agente

Todo lo que un agente lee sale de su contexto, así que una respuesta que se
repite es un costo real que no compra nada. Hay dos topes para eso, y los dos
aplican **solo a MCP**: la web y la terminal dibujan todos los servicios y todos
los frames sin costo para nadie, y una capacidad que se comporta distinto según
el cliente es una que nadie puede razonar.

- **Las lecturas iguales se dicen una vez.** `diagnose_silence` sobre un proyecto
  de veintidós servicios en silencio devolvía veintidós copias del mismo párrafo,
  distintas solo en la dirección incrustada en cada frase: unos 6.900 tokens, con
  el 96% en las entradas por servicio. Ahora agrupa los servicios cuya lectura es
  idéntica, dice las frases compartidas una sola vez con `{listen}`, `{point_at}`,
  `{expects}` y `{upstream}` representando el campo de cada miembro, y sube a
  `same_for_all` los hechos en los que todos coincidieron. El mismo informe son
  unos 2.100 tokens. **No se pierde nada**: cada frase original se puede
  reconstruir con los marcadores y los campos que van al lado, y una lectura que
  de verdad difiere —dos servicios capturando cantidades distintas de llamadas—
  queda separada en vez de plegarse con la del vecino.
- **Los textos y las listas largas dicen qué dejaron fuera.** Un texto de más de
  2.000 caracteres se corta, y una lista de más de 24 entradas conserva los dos
  extremos con un marcador en el medio que dice cuántas faltan. Los dos extremos,
  no las primeras 24: el desenlace de un stream está al final, así que quedarse
  con la cabeza dejaría fuera justo lo que se está depurando. `detail: true` en
  `get_call` devuelve todo, completo.

Ninguno de los dos topes es un resumen. Un resumen decide por el lector qué
servicios importan, y el lector es el que está depurando.

### Qué no está en MCP, a propósito

- **Borrar un proyecto.** `remove_service` cubre el servicio que hay que sacar, y
  volver a conectar el mismo proyecto es como se aplica una configuración que
  cambió, así que nada queda trabado detrás de esa falta. Tirar un proyecto
  entero — sus servicios, sus esquemas, lo que haya adentro — es una decisión con
  una mano humana encima, y el botón está en la interfaz web.
- **El flujo en vivo.** `wait_for_call` responde lo mismo con un límite de
  tiempo, y mantener un stream abierto durante una llamada de herramienta no le
  aporta nada a un agente.
- **Instalar la autoridad certificadora.** `trust_certificate` entrega el
  certificado y los comandos exactos: el certificado público no es un secreto, y
  una respuesta sobre la que quien lee no puede actuar no es una respuesta.
  Ejecutar uno de esos comandos modifica el almacén de confianza de la máquina, y
  ese acto sigue siendo del usuario.
- **Desactivar el filtrado de credenciales.** No existe esa opción en ninguna
  parte, y MCP sería la última superficie en tenerla.

### Las credenciales no salen

Todo lo anterior se filtra antes de salir, con dos huecos que se nombran al final
de esta sección. `Authorization`, `Cookie`,
`X-Api-Key`, `password`, `client_secret` y sus distintas grafías vuelven como
`[redacted by Sonda]` — en cabeceras, en cuerpos, y dentro de un JSON anidado en
un cuerpo. **No hay opción para desactivarlo**, a propósito: una bandera para eso
se enciende probando contra un proyecto de juguete y se olvida encendida contra
uno real. La interfaz web muestra los payloads de aplicación almacenados porque
allí quien lee es el usuario; los secretos de handshake de Postgres y AMQP ya no
están en ninguna superficie porque se borraron antes de persistirlos.

Comparar un nombre de campo solo funciona sobre un campo que tiene nombre, así
que hay cuatro pasadas más que llegan donde eso no alcanza. Cada una corre en un
lugar conocido de la respuesta y es inalcanzable desde cualquier otro: el
endpoint que la herramienta llamó es lo que indica qué campos son de Sonda, de
modo que un cuerpo capturado que casualmente lleve una clave `sql`, `detail` o
`postgres` queda exactamente como se registró.

- **Cadenas de consulta**, en cualquier lugar donde aparezca una URL: la ruta
  capturada, un redirect `Location`, un enlace dentro de un cuerpo.
  `?access_token=`, `?code=` y `?X-Amz-Signature=` se borran y el resto de la
  URL se conserva, porque la ruta es como reconoces la llamada.
- **Postgres**, que es un protocolo orientado a columnas: el nombre sensible y
  el valor sensible llegan en mensajes distintos. Un `RowDescription` se alinea
  contra los `DataRow` que vienen después, y una sentencia que nombra una
  credencial vuelve con su estructura intacta y sus literales borrados —
  incluido el resumen de una línea que el listado muestra antes de que hayas
  pedido nada, y los dos lugares donde una traza repite esa misma línea. El
  árbol dibujado como texto no se escanea buscando esa línea: cada nodo informa
  en qué se convirtió su propia lectura y se sustituyen las cadenas exactas, así
  que quedan cubiertos todos los nodos a cualquier profundidad.
- **La autenticación AMQP**, cuyo desafío y respuesta son cadenas de bytes
  opacas y no campos con nombre. Sonda borra la respuesta SASL de
  `connection.start-ok` y ambos lados de `connection.secure` antes de guardar.
  Si una trama queda incompleta y podría contener una credencial, no conserva
  ninguno de sus bytes sin procesar. El nombre del mecanismo seleccionado, como
  `PLAIN`, sigue visible. El reenvío siempre usa los bytes originales.
- **Una credencial que cambió, dentro de una comparación.** `diff_calls`
  direcciona el campo que cambió mediante una ruta, así que el nombre viaja como
  valor y las claves a su alrededor son `path`, `a` y `b`. Cuando esa ruta nombra
  una credencial, los dos lados de la comparación se borran.
- **La segunda copia de una captura decodificada.** Una sesión de Postgres, un
  WebSocket, un flujo de eventos, una llamada gRPC y una unidad AMQP se sirven dos veces —
  decodificados, y byte a byte tal como cruzaron — y filtrar la primera copia no
  vale nada mientras la segunda está al lado. La copia literal se descarta allí
  donde la vista decodificada la reemplaza, lado por lado. Donde nada la
  decodifica se conserva: la petición de un flujo de eventos, una trama gRPC
  comprimida y cualquier vista que quedó vacía — una página HTML 502 servida como
  `text/event-stream` sigue siendo el único registro de lo que pasó, y
  descartarla te dejaría sin nada en vez de con menos.

Quedan dos huecos, ambos deliberados. Un campo protobuf decodificado **sin**
esquema tiene un número y no un nombre, así que no hay nada que comparar y su
valor vuelve en claro; entrega al proyecto un descriptor set, o habilita
reflection en el servicio, y el campo recupera su nombre y se filtra como
cualquier otro:
`schema_status` dice cuál de los dos casos estás viendo. Y el mensaje de error de
un servicio — un error de transporte, un estado gRPC — vuelve
tal como se escribió: leer prosa como si fuera SQL la corta en el primer
apóstrofo, y borrar cualquier línea que nombre una credencial hace perder
`Internal: couldn't refresh the session cookie` justo en la herramienta que
existe para mostrar fallos.

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

