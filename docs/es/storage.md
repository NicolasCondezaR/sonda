[← Docs](README.md)

# Almacenamiento, comportamiento y costo

## Qué guarda Sonda, y qué implica eso

Sonda escribe en un archivo SQLite los bytes que cruzaron el cable. **Eso
incluye lo que sea que lleve tu tráfico**: cabeceras `Authorization`, cookies de
sesión, claves de API, datos personales. Los payloads de aplicación no se
redactan al entrar, y es una decisión deliberada y no un descuido: redactar una
captura que se puede reenviar significaría que ya no es lo que se envió.

Dos excepciones del handshake de conexión toman el balance más seguro: la
contraseña de **Postgres** y los **desafíos y respuestas SASL de AMQP** se borran
de la copia almacenada mientras los bytes pasan. Ninguna de esas capturas se
puede reenviar sin el estado de conexión que ya desapareció, y la alternativa
es una credencial viva en un archivo de texto plano. Ver
[PostgreSQL](protocols.md#postgresql) y [AMQP 0-9-1](protocols.md#amqp-0-9-1).

El otro lugar donde las credenciales sí se retienen es [el servidor
MCP](agents.md), porque ahí las respuestas salen de la máquina.

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

