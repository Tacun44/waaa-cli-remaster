# Operación, datos locales y rendimiento

`wa-cli` es un cliente de WhatsApp para la terminal. Usa `whatsmeow` para
conectarse a WhatsApp, Cobra para los comandos y Bubble Tea para la interfaz
de pantalla completa que se abre al ejecutar `wa` sin subcomando.

## Qué hace cada modo

- `wa`: interfaz interactiva. Muestra los 100 mensajes más recientes del chat
  actual. `PgUp` carga 100 mensajes anteriores cuando se alcanza el inicio;
  `PgDn` vuelve hacia los más recientes.
- `wa watch`: mantiene una conexión abierta, imprime los mensajes entrantes y
  actualiza el índice e historial local.
- `wa chat …`, `wa contact …`, `wa group …` y `wa media …`: comandos breves
  que sirven tanto para uso manual como para scripts con `--json`.

No es un cliente oficial de Meta. Su sesión se vincula al teléfono mediante
`wa login` y depende del protocolo y las restricciones de WhatsApp.

## Datos locales

Por defecto los datos viven en `~/.config/wa/`; la ruta efectiva se puede ver
con `wa config get dataDir`.

| Archivo | Propósito | Tratamiento |
| --- | --- | --- |
| `data/session.db` | Credenciales y estado de dispositivo de whatsmeow. | No editar ni compartir. Se elimina al hacer `wa logout`. |
| `data/session.db-wal` | Registro temporal de SQLite de la sesión. | Normal mientras la app está activa. |
| `data/messages.db` | Historial local de mensajes, con índices por chat y fecha. | Base que usa la app para abrir chats con rapidez. |
| `data/messages.json` | Historial de versiones anteriores. | Se importa una vez a `messages.db` y queda como respaldo; ya no se consulta. |
| `data/chats.json` | Índice de chats: nombre, JID, último mensaje, no leídos y silencios. | Caché local; no es un espejo completo de WhatsApp. |
| `data/known_recipients.json` | Destinatarios ya confirmados para el protector de envíos. | Datos de control, no contenido de mensajes. |
| `data/tui.log` | Registro técnico de la interfaz. | Útil para diagnóstico; puede incluir metadatos, así que no compartir sin revisar. |

Los mensajes se almacenan localmente para permitir `wa chat open`, responder,
reenviar, descargar medios y usar la TUI sin una sincronización completa en
cada acción. Los medios no se guardan de forma permanente por abrirlos: se
descargan bajo demanda a archivos temporales o a `~/Downloads` cuando se usa
la descarga explícita.

## Rendimiento

La TUI pagina el historial: primero carga 100 mensajes y solo incorpora otra
página al navegar hacia arriba. SQLite evita que recibir o guardar un mensaje
requiera leer, decodificar y reescribir el historial completo, que era el
coste principal del antiguo JSON.

Las fotos y stickers no se renderizan todos dentro de la terminal. `Ctrl+O`
permite recorrer las fotos cargadas con `↑`/`↓` y abrir la elegida con
`Enter`. `Ctrl+U` hace lo mismo con los videos y los reproduce. Esto evita
que el renderizador de texto frene el desplazamiento. `u` conserva el acceso
rápido al video más reciente y `l` abre el enlace más reciente.

En Linux, las fotos se abren con `imv` cuando está instalado; `Ctrl+R` las
rota 90° en sentido horario. Si `imv` no está disponible, se usa el visor
asociado del sistema.

La primera operación que acceda al historial tras actualizar puede tardar más:
importa el `messages.json` heredado a `messages.db` una sola vez. No cierres el
proceso durante esa importación. Cuando finalice, el archivo JSON queda como
respaldo y el acceso normal usa SQLite.

## Atajos de la TUI

| Tecla | Acción |
| --- | --- |
| `PgUp` / `PgDn` | Recorrer historial; `PgUp` carga otra página al llegar arriba. |
| `Ctrl+O` | Elegir una foto cargada (`↑`/`↓`) y abrirla con `Enter`. |
| `Ctrl+U` | Elegir un video cargado (`↑`/`↓`) y reproducirlo con `Enter`. |
| `u` | Abrir el video más reciente directamente. |
| `l` | Abrir el enlace más reciente. |
| `Ctrl+F` | Buscar en los mensajes cargados. |
| `Ctrl+E` | Adjuntar documentos mediante selector gráfico. |
| `Ctrl+H` | Descargar y abrir el documento más reciente. |
| Arrastrar archivo | Soltar uno o más archivos sobre la terminal para enviarlos al chat seleccionado; JPEG/PNG/WebP/GIF se envían como imágenes y el resto como documentos. |
| `Ctrl+V` | Enviar una imagen desde el portapapeles. |
| `T` / `Ctrl+T` | Elegir una nota de voz para transcribir. |
| Mantener `Espacio` en el cuadro de mensaje | Dictar texto con Whisper local. Aparece una transcripción provisional mientras hablas; al soltar, se corrige con una pasada final. Revisa el borrador antes de enviarlo. `Esc` cancela. |
| `r` / `Ctrl+R` | Grabar una nota de voz para enviar. |
| `p` | Elegir una nota de voz para reproducir. |
| `Ctrl+D` | Elegir un mensaje propio para eliminar para todos. |
| `b` | Mostrar u ocultar la barra lateral. |
| `?` | Ver la ayuda completa. |

## Seguridad y operación responsable

- Protege `~/.config/wa/data/session.db` como una credencial: quien pueda
  copiarlo puede comprometer la sesión vinculada.
- Revisa la pantalla de confirmación al escribir a un destinatario nuevo. Los
  límites por minuto, hora y día reducen envíos accidentales; se ajustan con
  `wa config set`.
- `wa extension install` clona un repositorio y `wa extension run` ejecuta su
  punto de entrada como un proceso local con tus mismos permisos. Instala solo
  extensiones que confíes y revisa su código antes de ejecutarlas.
- WhatsApp solo permite una conexión activa por dispositivo. Si `wa watch` o
  la TUI están abiertos, usa `wa chat list --no-sync` para leer la caché en vez
  de iniciar otra sincronización.
- Antes de compartir un diagnóstico, revisa `tui.log`, `chats.json` y las
  exportaciones `--json`: pueden contener nombres, JIDs, contenido o previews.

## Mantenimiento y diagnóstico

Si la TUI aparece desplazada, muestra texto viejo o deja espacio vacío debajo
del marco, cierra y vuelve a abrir `wa-cli` para cargar el binario instalado.
La interfaz consulta el tamaño de la terminal cada cinco segundos y ajusta el
marco cuando cambia. El encabezado de la barra lateral debe decir `Chats`.
El diagnóstico y la corrección del incidente visual están en
[TUI_LAYOUT_2026-09-28.md](./TUI_LAYOUT_2026-09-28.md).

```sh
wa status                         # comprobar que la sesión está vinculada
wa config get dataDir             # encontrar los datos locales
wa chat list --no-sync            # leer la caché sin abrir otra conexión
du -sh ~/.config/wa/data          # tamaño del estado local (Linux)
```

No borres `session.db` manualmente. Usa `wa logout` si quieres desconectar la
cuenta. Si necesitas investigar una lentitud, conserva una copia de los datos
antes de realizar limpieza manual y revisa el tamaño de `messages.db`,
`session.db-wal` y `tui.log`.
