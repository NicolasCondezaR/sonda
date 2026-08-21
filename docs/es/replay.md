[← Docs](README.md)

# Replay y diff

Al seleccionar una llamada, el inspector ofrece **REPLAY**. La request sale de
nuevo construida desde los bytes que se guardaron, así que lo que llega al
servicio es lo mismo que llegó la primera vez.

Se envía **a través de Sonda** y no directo al upstream, lo que significa que
el reenvío se captura como cualquier otro tráfico: aparece en el campo, queda
enlazado a la llamada de la que salió, y ambas se pueden comparar de inmediato.

```bash
# replay a call onto the channel it came from
curl -X POST http://127.0.0.1:9000/api/calls/42/replay

# or onto another configured channel, to ask the same request of a second instance
curl -X POST http://127.0.0.1:9000/api/calls/42/replay \
  -H 'Content-Type: application/json' -d '{"target":"orders-staging"}'

curl 'http://127.0.0.1:9000/api/diff?a=42&b=43'
```

El replay solo apunta a un **canal configurado**. No existe un modo de URL
arbitraria: el caso útil es hacerle la misma request a otra instancia que ya
estás observando, y cualquier cosa más amplia convierte un depurador en un
forjador de requests.

**Una captura truncada no se puede reenviar, y Sonda se niega en vez de
intentarlo.** Solo se guardó la cabeza del cuerpo, así que lo que saldría no
sería lo que se capturó, y el resultado llevaría la palabra "replay" siendo una
request distinta. La negativa nombra el arreglo: subir `max_body_bytes` y volver
a capturar.

### El diff es estructural

Los cuerpos se comparan como estructuras parseadas, no como texto. Reordenar
claves o reindentar no son diferencias, así que la respuesta es una lista corta
de rutas en vez de un muro de rojo y verde:

```
~ qty          a 3          b 7
+ nota                      b urgente
```

- El orden de un arreglo **sí** es una diferencia —la posición tiene significado
  en un campo `repeated` de protobuf— mientras que el orden de las claves de un
  objeto no lo es.
- `1290000` y `"1290000"` son el mismo valor. protojson emite int64 como string,
  y reportar eso sería una afirmación sobre codificación, no sobre datos.
- Los streams de gRPC se comparan mensaje por mensaje, así que el mensaje 3 de 5
  puede diferir mientras el resto calza.
- La duración se excluye a propósito: cambia en cada replay y enterraría las
  diferencias que sí significan algo.
- Cuando un lado no es JSON, o no tiene esquema, el diff lo dice e informa si
  los bytes coinciden, en vez de inventar una comparación estructural.


## Comparar dos corridas

Comparar dos llamadas responde "por qué esta petición falló y esta otra no". No
responde la pregunta con la que la gente llega de verdad, que es **"ayer
funcionaba y hoy no"** — porque un flujo es un árbol de llamadas, y lo que
cambió suele estar varios saltos más abajo, o es una llamada que dejó de
ocurrir.

Se le da una llamada de cada corrida y encuentra el resto de ambos árboles:

```
GET /api/flowdiff?a=1204&b=1731
```

En la interfaz: abre una llamada de la corrida que funcionó, presiona **HOLD
RUN**, después abre una llamada de la que falló y presiona **DIFF FLOW**. En el
cliente de terminal son los mismos dos pasos con `x` y `x`. Un agente llama a
`diff_flows`.

```
4 matched, 1 only in a, 0 only in b
first divergence: gateway http POST /orders/{} → ms-rates http GET /rates/{}

gateway http POST /orders/{}                                   same
├─ ms-rates http GET /rates/{}                                 changed
│      status: 200 → 500
│  └─ ms-schedules http GET /schedules/{}                      same
└─ ms-billing http POST /invoices                              only in a — this call is no longer made
```

### Cómo se alinean dos corridas

Ni por id ni por trace id: eso es justamente lo que cambia entre corridas. Dos
llamadas son la misma llamada cuando coinciden en **servicio, protocolo, método
y la forma de la ruta**, y después por su posición entre los hermanos que
comparten esa firma.

- **La forma de la ruta** significa que `/orders/ORD-1` y `/orders/ORD-2` son
  una sola llamada. Un segmento pasa a comodín cuando es todo dígitos, un UUID,
  una cadena hexadecimal larga, o un identificador con separador.
  `normalize=loose` además comodiniza cualquier segmento que contenga un dígito,
  lo que aplana `/v2/` y `/v3/` en la misma ruta; `normalize=off` compara las
  rutas literalmente.
- **gRPC no necesita nada de eso.** Su ruta es `/paquete.Servicio/Método` y no
  lleva valores, así que la firma es exacta.
- **GraphQL se alinea por operación**, porque toda petición GraphQL de un
  proyecto es un POST al mismo endpoint y emparejar por ruta juntaría una
  mutación con una consulta.

### Mira `unmatched` antes de creerle al resto

Una comparación donde la mayoría de las llamadas no encontró pareja no encontró
diferencias reales: encontró una forma de ruta que no se está reconociendo. Eso
es la perilla, no un hallazgo, y la respuesta lo dice en vez de presentar una
lista de llamadas faltantes como si el sistema hubiera cambiado.

Vienen otras dos señales de honestidad en cada respuesta. `same_entry` es falso
cuando las dos semillas ni siquiera eran la misma llamada, lo que vuelve
irrelevante todo lo de abajo. `certain` es falso cuando alguna de las corridas se
agrupó por tiempo en vez de por un trace id real — una diferencia entre dos
adivinanzas no es la misma afirmación que una diferencia entre dos hechos.

### Qué se compara y qué no

Por par alineado: el estado, si falló, el detalle del fallo, y si un lado se
respondió desde una grabación. La duración se deja fuera a propósito, por la
misma razón que en el diff de una llamada: cambia en cada corrida y marcaría
todos los nodos.

Los cuerpos se comparan solo en la divergencia y sus hijos directos por defecto,
con el mismo diff estructural de más arriba. `bodies=all` compara todos los pares
alineados, lo que en un flujo ancho son decenas de lecturas de payload y un muro
de JSON; `bodies=none` los omite.

Una limitación que conviene conocer: los hermanos que comparten firma se
emparejan por posición. Dos corridas que hicieron las mismas tres llamadas en
distinto orden se emparejan mal.
