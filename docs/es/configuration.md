[← Docs](README.md)

# Proyectos y configuración

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
    protocol: http     # http, grpc, postgres o amqp

  - name: payments
    listen: 127.0.0.1:9103
    upstream: https://api.payments.example.com  # verificado como lo haría cualquier cliente
    protocol: http
    tls: true                    # responder este puerto con certificado, para clientes que rechazan http://
    insecure_skip_verify: false  # por servicio, nunca global. Ver la sección TLS

  - name: events
    listen: 127.0.0.1:9401
    upstream: amqp://127.0.0.1:5672  # amqps:// para un broker con TLS
    protocol: amqp
```

Después apunta a `127.0.0.1:9102` lo que sea que llame a `admin-api`. El mismo
binario y el mismo archivo sirven para servicios en contenedores y para
servicios corriendo de forma nativa, que es justamente el punto: un stack local
de verdad suele ser las dos cosas.

Dentro de Docker, usa `host.docker.internal` para alcanzar un servicio que corre
en el host. Ver `sonda.docker.yaml`.

