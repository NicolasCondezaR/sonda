[← Docs](README.md)

# API

| Método y ruta | Para qué |
|---|---|
| `GET /api/calls` | Lista las capturas, más recientes primero. Filtros: `target`, `method`, `path`, `status`, `protocol`, `grpc_status`, `failed`, `q`, `since`, `until`, `limit`, `before_id`. `failed=true` devuelve solo las fallidas, `failed=false` solo las que no fallaron, y omitirlo devuelve ambas. |
| `GET /api/calls/{id}` | Una captura con cabeceras y cuerpos, además de la vista decodificada específica del protocolo (`grpc`, `socket`, `stream`, `graphql`, `postgres` o `amqp`) cuando corresponde. |
| `GET /api/targets` | Los targets configurados. |
| `GET /api/schemas` | Por cada target gRPC: qué fuente de esquema resolvió, o por qué ninguna. |
| `POST /api/calls/{id}/replay` | Reenvía la llamada, opcionalmente a otro canal. |
| `GET /api/diff?a=&b=` | Comparación estructural de dos llamadas. |
| `GET /api/trace?call=` | La petición completa a la que perteneció una llamada, como árbol. |
| `GET /api/stub` | Qué servicios están respondiendo desde grabaciones. |
| `POST /api/stub` | Activar o desactivar el stub de un servicio, o limpiarlo todo. |
| `GET /api/faults` | Qué servicios se están rompiendo a propósito, y cómo. |
| `GET /api/drift?target=` | Si un endpoint sigue respondiendo la forma que respondía. |
| `POST /api/faults` | Poner o quitar una regla de fallo. |
| `GET /api/projects` | Los proyectos, sus servicios y qué está escuchando de verdad. |
| `POST /api/projects` | Crear uno. `PATCH`/`DELETE /api/projects/{id}` renombran y borran. |
| `POST /api/projects/{id}/activate` | Cierra los puertos del proyecto actual y abre los de este. |
| `POST /api/projects/deactivate` | Cierra todos los puertos. No borra nada, y activar lo devuelve todo. |
| `POST /api/projects/{id}/descriptor` | Sube los esquemas compilados de todo el proyecto. |
| `POST /api/projects/{id}/services` | Agrega o actualiza un servicio. `DELETE /api/services/{id}` quita uno. |
| `POST /api/discover` | Lee servicios de un `.env` o un compose sin guardar nada. |
| `GET /api/runtime` | Qué proyecto está activo y qué está escuchando de verdad, incluyendo cuántas conexiones aceptó cada puerto. |
| `GET /api/diagnose` | Por qué no se está capturando nada, servicio por servicio. Solo lee lo que Sonda ya sabe y no toca la red. |
| `POST /api/diagnose` | El mismo informe, más una conexión TCP a cada upstream. Es un efecto secundario, y por eso no está en el `GET`. |
| `GET /api/tls` | La autoridad certificadora: el certificado mismo en `certificate_pem`, los comandos exactos para confiar en ella y para quitarla, y —cuando Sonda corre en un contenedor— el `docker cp` que saca el archivo. Nunca la clave privada. |
| `GET /api/tls/ca.pem` | Descarga el certificado de la CA. Útil cuando Sonda corre en un contenedor. |
| `GET /api/stats` | Cantidad de capturas, rango de tiempo y llamadas descartadas bajo carga. |
| `GET /api/stream` | Eventos server-sent: cada captura en el momento en que se guarda. Es lo que lee el campo en vivo. |
| `GET /health` | Liveness. |

El listado no lleva cuerpos a propósito: unos cientos de llamadas con payloads
adjuntos es inusable. Los cuerpos vienen del endpoint de detalle, como `text`
cuando el contenido es UTF-8 válido y como `base64` cuando no lo es. La API
nunca adivina: informa qué son los bytes.

`q` busca en rutas y en payloads de texto. Se trata como una frase literal, así
que `"sku":"ABC-9"` y `/v1/orders` funcionan tal como se escriben en vez de
leerse como operadores de consulta.

