[← Docs](README.md)

# Hoja de ruta

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
| 19 | AMQP 0-9-1 y AMQPS: reenvío byte a byte, unidades de captura útiles, sanitización SASL, búsqueda y vistas decodificadas en API/MCP/web/TUI | listo |
| 20 | Diff de flujos y el trigger: dos corridas alineadas hasta la llamada donde se separaron, y una condición armada para atrapar lo que pasa mientras nadie mira | listo |
| 21 | Un id de traza propio de Sonda, escrito sobre una petición que llegó sin ninguno, para que lo que causó se agrupe con exactitud en vez de adivinarse — marcado en cada captura que lo lleva, para que nunca se confunda con la instrumentación del cliente | listo |

Kafka falta de esa tabla a propósito. Por qué, va abajo.

### Por qué falta Kafka

**Hoy Sonda no tiene ningún listener de Kafka.** `protocol:` acepta `http`,
`grpc`, `postgres` y `amqp`; las tramas de Kafka que lleguen a cualquiera de
esos listeners no se convierten en capturas Kafka. Nada de esta sección se puede
usar todavía: es la explicación de por qué falta la fila, no una receta.

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
- El soporte de AMQP cubre solo 0-9-1. Las capturas son unidades de protocolo
  por dirección, no transacciones reconstruidas ni pares de publicación y
  entrega, y no se pueden reenviar, usar como stub ni someter a inyección de
  fallos.
- Las tramas AMQP que superan el límite de 16 MiB del analizador de captura se
  reenvían, pero solo se guarda su tamaño y el error. El límite normal de
  almacenamiento `max_body_bytes` también se aplica a las unidades AMQP.
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

