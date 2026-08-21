[← Docs](README.md)

# Instalación

## Instalación

Elige la que ya tengas. Nada de esto necesita un toolchain de C ni un SQLite en
el sistema: el driver es Go puro, que es también la razón de que los binarios
sean estáticos y la imagen pese 50 MB.

| | |
|---|---|
| **macOS** | `brew install NicolasCondezaR/tap/sonda` |
| **Windows** | `scoop bucket add nicolascondezar https://github.com/NicolasCondezaR/scoop-bucket`<br>`scoop install sonda` |
| **Go** | `go install github.com/NicolasCondezaR/sonda/cmd/sonda@latest` |
| **Docker** | `docker run -p 127.0.0.1:9000:9000 -v sonda:/data ghcr.io/nicolascondezar/sonda` |
| **Binario** | Descárgalo desde [Releases](https://github.com/NicolasCondezaR/sonda/releases), descomprime y ejecuta |
| **Fuente** | `git clone` y `go build ./cmd/sonda` |

En Linux usa `go install`, la imagen o el tarball: las casks de Homebrew son
solo para macOS.

La línea de Docker publica en `127.0.0.1` y no en todas las interfaces, igual
que el resto de este documento: `sonda.db` guarda las credenciales que hayan
cruzado el cable, en texto plano y sin ningún login delante. Dentro del
contenedor Sonda escucha en `0.0.0.0` —ahí el aislamiento lo pone el
contenedor—, que es lo que hace `-api-listen` en el comando de la imagen; fuera
de un contenedor el valor por omisión sigue siendo loopback.

Esa línea de Docker publica la interfaz y nada más, lo que alcanza para mirar y
no alcanza para capturar: **un proxy necesita su propio puerto publicado por
cada servicio**, porque el puerto al que se conecta un cliente es todo el
mecanismo. Declarar un servicio en el 9101 y descubrir después que nada en tu
máquina lo alcanza es la media hora de confusión que este párrafo existe para
evitar.

```bash
docker run -p 127.0.0.1:9000:9000 -p 127.0.0.1:9101:9101 \
  -v sonda:/data ghcr.io/nicolascondezar/sonda
```

`docker compose up` ya publica el 9101 y el 9201, y por eso el inicio rápido de
abajo funciona sin decir nada de esto.

Los archivos del release traen cuatro binarios, no uno: `sonda`, el cliente de
terminal `sonda-tui`, y los dos servicios de juguete `echo` y `grpcdemo` que usa
el inicio rápido de abajo — para tener algo que capturar sin levantar nada
propio. Los gestores de paquetes instalan solo los dos primeros: un binario
llamado `echo` en el PATH no tiene por qué tapar al del sistema.

```bash
sonda            # el proxy y la interfaz, en http://127.0.0.1:9000
sonda -version   # qué build es este
sonda-tui        # el cliente de terminal
```

Bajar el tarball a mano en macOS tiene un paso extra: Gatekeeper pone en
cuarentena cualquier binario sin firmar que llegue por el navegador. O lo bajas
con `curl`, o limpias la marca una vez. Homebrew lo hace por ti.

```bash
xattr -dr com.apple.quarantine sonda sonda-tui echo grpcdemo
```

### Iniciar Sonda al entrar a Windows

En Windows, Sonda puede instalar una tarea del Programador de tareas para el
usuario actual. Se ejecuta con privilegios normales, solo después de que ese
usuario inicia sesión, y no almacena contraseñas:

```powershell
sonda autostart install -config "$HOME\.sonda\sonda.yaml"
sonda autostart status
```

`install` crea la tarea o la reutiliza solamente cuando su definición completa
aún coincide con el usuario y la configuración actuales. Nunca reemplaza una
tarea ajena o modificada que ocupe el nombre determinístico de Sonda. Inicia de
inmediato la tarea verificada y espera `/health`; no es necesario reiniciar para
probarla. El ciclo habitual es:

```powershell
sonda autostart start
sonda autostart stop
sonda autostart restart
sonda autostart uninstall
```

Si la instalación usó una ruta de configuración no predeterminada, pasa el
mismo `-config PATH` a `status`, `start`, `stop`, `restart` y `uninstall`. Estos
comandos derivan localmente la tarea esperada en vez de confiar en metadata
almacenada dentro de la tarea.

La tarea fija como rutas absolutas la configuración y el directorio de trabajo,
funciona con batería, mantiene una sola instancia, no tiene el límite de 72
horas, se inicia cuando vuelve a estar disponible y reintenta tres veces, con
intervalos de un minuto, si el proceso falla. Cuando el binario en ejecución es
el target exacto `apps/sonda/current` de un shim verificable de Scoop, usa ese
shim para que `scoop update sonda` no deje la tarea apuntando a una versión
antigua. Un binario ejecutado desde un ZIP o un build local se considera
portable: colócalo en su ubicación definitiva antes de instalar el inicio
automático. Si después lo mueves, restaura la ruta original o elimina manualmente
la tarea antigua en el Programador de tareas antes de volver a instalar; Sonda
no sobrescribe una acción que cambió.

La detención señala únicamente la instancia de Sonda identificada por esta
tarea y permite drenar la cola de capturas. Si la señalización limpia falla, la
tarea canónica no cambió y su evento de control esperado todavía demuestra que
el proceso gestionado sigue activo, el único fallback es finalizar esa tarea
programada exacta; el estado informa que la detención fue abrupta. Sonda nunca
mata todos los procesos llamados `sonda.exe`.

El inicio automático rechaza una configuración cuyo `api_listen` no sea
loopback. El override explícito `-allow-non-loopback` existe para entornos
aislados, pero expone una API sin autenticación que puede leer capturas y
cambiar el estado del proxy. `uninstall` elimina solamente la tarea; conserva
la configuración, la base de datos, la autoridad certificadora y los logs. Los
logs se escriben junto a la configuración en `sonda.log`, con una copia rotada
y acotada.

## PowerShell

PowerShell 5.1 reescribe las comillas al pasar argumentos a ejecutables
externos, de modo que `curl.exe` corrompe los cuerpos JSON en silencio:

```powershell
# WRONG — the upstream receives {sku:ABC-9}, quotes stripped
curl.exe -X POST -H "Content-Type: application/json" -d '{"sku":"ABC-9"}' http://127.0.0.1:9101/echo
```

Usa `Invoke-RestMethod`, o pon el cuerpo en un archivo:

```powershell
# Sends a body, and reads what Sonda captured
$body = @{ sku = 'ABC-9'; qty = 3 } | ConvertTo-Json -Compress
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:9101/echo -ContentType 'application/json' -Body $body

(Invoke-RestMethod -Uri http://127.0.0.1:9000/api/calls).calls |
  Select-Object id, method, path, status, duration_ms | Format-Table

$id = (Invoke-RestMethod -Uri 'http://127.0.0.1:9000/api/calls?q=ABC-9').calls[0].id
(Invoke-RestMethod -Uri "http://127.0.0.1:9000/api/calls/$id").request.text
```

```powershell
# curl.exe is fine when the body comes from a file
curl.exe -X POST -H "Content-Type: application/json" --data-binary '@body.json' http://127.0.0.1:9101/echo
```

