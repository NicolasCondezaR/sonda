# Documentación de Sonda

[← Volver al proyecto](../../README.es.md) · *[In English](../README.md)*

## Ponerla a andar

| | |
|---|---|
| [Instalación](install.md) | Todas las rutas de instalación y las notas de PowerShell para Windows |
| [Proyectos y configuración](configuration.md) | Leer el `.env` o el compose de un proyecto, y el archivo de configuración |
| [Interfaces](interface.md) | La interfaz web y el cliente de terminal |
| [No captura nada, ahora qué](troubleshooting.md) | La lista de chequeo para un servicio apuntado a Sonda que no muestra nada |

## Qué entiende

| | |
|---|---|
| [Protocolos](protocols.md) | gRPC, TLS, PostgreSQL, AMQP 0-9-1, GraphQL, sockets y flujos de eventos |
| [Almacenamiento, comportamiento y costo](storage.md) | Qué se escribe, qué se blanquea, cuánto cuesta guardarlo |

## Qué hace con una captura

| | |
|---|---|
| [Replay y diff](replay.md) | Reenviar una llamada grabada y comparar dos de forma estructural |
| [Experimentos](experiments.md) | Modo stub, romper un servicio a propósito y deriva de contratos |
| [Agentes de código](agents.md) | El servidor MCP, los árboles de llamadas, y qué puede y no puede ver un agente |
| [API HTTP](api.md) | La API que leen todos los clientes, incluidos los de Sonda |

## Sobre el proyecto

| | |
|---|---|
| [Trabajo relacionado](comparison.md) | Las otras herramientas que capturan gRPC, y cuándo usarlas en vez de esta |
| [Hoja de ruta](roadmap.md) | Qué está hecho y qué viene |
| [Contribuir](../../CONTRIBUTING.md) | Tests, benchmarks, layout, protos, commits |
| [Seguridad](../../SECURITY.md) | Reportar algo que no debería ser público |
