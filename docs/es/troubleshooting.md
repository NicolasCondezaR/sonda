[← Docs](README.md)

# Lo apunté a mi servicio y no veo nada

Este es el primer uso más común de una herramienta así, y todas sus causas se
ven iguales desde afuera: una pantalla vacía. Por eso Sonda responde la pregunta
en vez de dejarla abierta. **Cuando no se capturó nada, el campo deja de estar
vacío y pasa a ser una lectura**: una línea por canal con lo que Sonda sabe de
él, y la misma respuesta está disponible en la terminal, por la API y para un
agente:

```bash
curl -s localhost:9000/api/diagnose | jq
```

```
sonda-tui              el inspector lo muestra mientras el campo está vacío
diagnose_silence       la herramienta MCP que llama un agente cuando falta una captura
```

### Qué puede decirte

Cada canal recibe un veredicto, y los números que lo sostienen están a la vista:

| Veredicto | Qué significa |
|---|---|
| `capturing` | Aquí se están registrando llamadas. Un campo vacío es el filtro, la ventana o el canal seleccionado, no el proxy |
| `listener_down` | El puerto nunca se abrió, casi siempre porque otra cosa lo tiene tomado. Aquí no puede llegar nada, y el error dice qué pasó |
| `connected_not_captured` | Algo llegó a este puerto y nunca se convirtió en una llamada. Sonda vio la conexión y no entendió lo que venía por ella |
| `upstream_unreachable` | El servicio detrás de Sonda rechazó una conexión cuando se le preguntó. Solo se informa después de un sondeo explícito |
| `no_connections` | Nada tocó este puerto desde que se abrió |

La lectura que más trabajo hace es **`connections`**, que cuenta cada conexión
TCP que el puerto aceptó, se haya convertido en llamada o no. Conexiones sin
capturas es un cliente que encontró a Sonda y fue malinterpretado: un cliente
hablando TLS contra un listener en claro o al revés, o un protocolo que Sonda no
proxea. Cero conexiones es un cliente que nunca llegó. Son problemas distintos
con soluciones distintas, y sin ese contador se leen exactamente igual.

Sonda actúa como proxy para HTTP —incluidas las actualizaciones a WebSocket y
los eventos enviados por el servidor—, gRPC, PostgreSQL y AMQP 0-9-1. Un cliente
de Kafka, de Redis o de TCP plano apuntado a un puerto de Sonda es aceptado y
nunca entendido, y eso aparece como `connected_not_captured` en vez de como
silencio.

### Qué no puede decirte, y lo dice

**Sonda no puede ver un cliente que nunca se conectó a ella.** Un puerto sin
conexiones se lee igual si quien llama sigue hablando directo con el servicio,
si está apuntado a otro puerto, o si simplemente todavía no se ejecutó. No hay
señal honesta que separe esas tres, así que el informe las nombra todas y
entrega lo único que sí las separa: apunta al cliente a Sonda, dispara la
llamada y mira el contador de conexiones. Se mueve incluso cuando la petición
está mal. Si se queda en cero, no está llegando nada a Sonda.

### Sondear un upstream es un efecto secundario

Averiguar si el servicio detrás de Sonda está arriba significa marcarlo, y eso
es tráfico que el usuario no envió. Por eso nunca ocurre solo: ni al cargar la
página, ni al refrescar, ni por un temporizador.

```bash
# solo lee lo que Sonda ya sabe, no toca la red
curl -s localhost:9000/api/diagnose

# además marca una vez cada upstream y corta
curl -s -X POST localhost:9000/api/diagnose
```

Pedirlo es la única forma: `PROBE UPSTREAMS` en el navegador, `p` en la
terminal, `probe_upstreams` en la herramienta MCP. La conexión no envía ningún
byte y va **directo al servicio, nunca por el listener de Sonda**, así que un
sondeo jamás puede aparecer en la lista de capturas como si fuera una llamada
tuya.

### Si sigue sin aparecer nada

- **¿Hay un proyecto activo?** Sin proyecto activo no hay puertos abiertos, y el
  informe lo dice antes que cualquier otra cosa.
- **¿El cliente releyó su configuración?** Un proceso que arrancó antes de
  cambiar la variable de entorno sigue con la dirección vieja.
- **¿El esquema es el correcto?** Un listener que termina TLS no responde nada
  en `http://`, y uno en claro no responde nada en `https://`. Por eso la línea
  que entrega cada servicio lleva el esquema.
- **Revisa el log de la propia Sonda.** Un handshake TLS rechazado se informa
  ahí y en ningún otro lado, porque falla antes de que exista una llamada a la
  que adjuntarlo.

