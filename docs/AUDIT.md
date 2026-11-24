# Auditoría técnica — 2026-08-20

## Alcance y método

Se revisó el árbol de código de `wa-cli`, sus flujos de comandos, el estado
local configurado en esta instalación y los puntos de ejecución de procesos o
escritura de archivos. Se ejecutaron `go vet ./...`, la suite `go test ./...`
y pruebas con detector de carreras. No se enviaron mensajes, no se alteró la
sesión de WhatsApp y no se evaluaron vulnerabilidades remotas porque el entorno
de auditoría no tiene salida de red.

## Resultado

La aplicación es un cliente CLI de WhatsApp en Go, no una aplicación web ni un
servicio en segundo plano. La conexión real la aporta `whatsmeow`; la TUI es
local y los comandos son procesos breves salvo `wa watch` y `wa`.

| Área | Resultado | Observación |
| --- | --- | --- |
| Compilación y análisis estático | Aprobado | `go vet ./...` no informó problemas. |
| Pruebas | Aprobado | La suite completa pasó, incluidas las pruebas de carrera. |
| Historial | Mejorado | `messages.db` usa SQLite e índices; la TUI pagina de 100 en 100. |
| Sesión | Riesgo gestionable | `session.db` y su WAL contienen estado sensible de la cuenta. |
| Extensiones | Riesgo aceptado | Una extensión es código externo ejecutado con permisos del usuario. |
| Dependencias | Pendiente de verificación externa | Se requiere red y un escáner como `govulncheck` para confirmar CVEs actuales. |

## Hallazgos

### Alto: extensiones equivalen a ejecutar código local

`wa extension install` acepta una fuente que `git clone` entiende y `wa
extension run` ejecuta su punto de entrada. El manifiesto reduce rutas inválidas
o escapes de directorio, pero no convierte código de terceros en código seguro.

**Acción operativa:** instalar únicamente repositorios confiables y revisar el
entrypoint antes de ejecutarlo.

### Alto: la sesión y el historial viven sin cifrado adicional de wa-cli

Los permisos de los archivos de datos son restrictivos cuando el sistema los
respeta, pero la aplicación no añade cifrado de contenido al `session.db` ni a
`messages.db`. Una cuenta de usuario comprometida puede leerlos.

**Acción operativa:** usar cifrado de disco y no copiar `~/.config/wa/data` a
lugares compartidos. Cerrar sesión con `wa logout` antes de desechar un equipo.

### Medio: primera migración del historial requiere tiempo y espacio temporal

El JSON heredado se importa a `messages.db` en la primera apertura del
historial. Durante ese momento existen ambos archivos, por seguridad y para
permitir recuperación. En esta instalación todavía no existe `messages.db`,
así que la migración está pendiente hasta la primera operación que abra un
chat o inicie la TUI.

**Acción operativa:** permitir que esa primera importación termine y conservar
el JSON como respaldo hasta verificar que el historial está completo.

### Medio: la caché de chats no es un respaldo completo de WhatsApp

`HistorySync` depende de lo que WhatsApp entregue como reciente; chats antiguos
o inactivos pueden no aparecer hasta recibir actividad. Esto está documentado
como limitación funcional, no como pérdida producida por la app.

### Bajo: documentación histórica debía actualizarse

La arquitectura aún describía `msgstore` como JSON aunque el código usa
SQLite. Esta auditoría actualiza la documentación de arquitectura y añade la
guía de operación.

## Límites de la auditoría

- No hubo análisis de CVEs actualizado: `go list -m -u all` no pudo contactar
  el proxy de Go debido a las restricciones de red del entorno.
- No se hizo una prueba de extremo a extremo contra WhatsApp para evitar
  modificar mensajes, sesión o datos remotos durante una auditoría.
- La protección de extensiones es una decisión de confianza del usuario, no
  un sandbox de seguridad.
