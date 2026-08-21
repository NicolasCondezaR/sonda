[← Docs](README.md)

# Experimentos

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

