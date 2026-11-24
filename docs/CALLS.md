# Llamadas de audio y video — implementación local, 2026-09-29

La TUI incorpora llamadas de audio 1:1 mediante `meowcaller`. Requiere Linux
con PipeWire/PulseAudio y los ejecutables `parec` y `pacat` (paquete `libpulse`
en Arch). El usuario confirmó una llamada saliente real con timbrado y audio
en ambos sentidos el 2026-09-29.

La versión local añade videollamadas 1:1 con cámara V4L2, `ffmpeg` y una ventana
de `mpv`. El camino de video de `meowcaller` aún no se ha validado mediante una
videollamada real entre dos cuentas; la prueba local de cámara, codificación y
decodificación no prueba la entrega por WhatsApp.

## Uso

1. Cierra y vuelve a abrir `wa-cli` después de actualizar el ejecutable.
2. Selecciona el chat individual y pulsa **F4** para abrir el panel.
3. **Enter** inicia una llamada de audio; **v** inicia una videollamada al
   contacto que aparece en el panel.
4. Para una llamada entrante, **F5** contesta y **F6** rechaza.
5. **F6** cuelga una llamada en curso. Dentro del panel, **m** silencia o
   reactiva el micrófono; durante una videollamada, **v** apaga o enciende la
   cámara. **Esc** cierra el panel sin terminar la llamada.

Una llamada entrante aparece en el pie de la TUI y genera una notificación
si las notificaciones están habilitadas. No abre un modal automáticamente,
para evitar que un Enter escrito en el chat conteste por accidente.

Si es de video, F5 la contesta con la cámara. La cámara se abre solo tras
contestar o después de que el destinatario acepta la videollamada saliente.
Solo se mantiene una llamada a la vez. Los grupos se contestan desde el
teléfono; la TUI no incluye llamadas desde `wa watch` ni un subcomando `wa call`.
Convertir una llamada de audio ya activa en video sigue pendiente: inicia una
videollamada nueva desde el panel.

## Audio

Se usan los dispositivos predeterminados de PipeWire/PulseAudio. Puedes
cambiar la entrada y salida de las pistas de `wa-cli` desde `pavucontrol`, o
fijarlos al iniciar el programa:

```sh
WA_CALL_SOURCE=nombre_de_fuente WA_CALL_SINK=nombre_de_salida wa-cli
```

Los nombres se consultan con `pactl list short sources` y
`pactl list short sinks`. No hay grabación de llamadas: el PCM circula en
memoria a 16 kHz mono. Dos colas acotadas evitan acumular audio retrasado.
La biblioteca usa tramas de 60 ms; no se ha medido la latencia final de red.
Usa auriculares para la primera prueba: no se ha añadido cancelación de eco.

## Video

`ffmpeg` captura `/dev/video0` a 640×480 y 15 fps y lo codifica como H.264.
El video remoto se abre en una ventana ligera de `mpv`, que puede girar la
imagen según la orientación enviada por el otro dispositivo. Una cola pequeña
descarta fotogramas antiguos si el sistema se atrasa y reanuda en un fotograma
clave. El video circula en memoria: no se guarda una grabación.

Para usar otra cámara, inicia el programa con `WA_CALL_CAMERA=/dev/video1`
(comprueba antes cuál corresponde a la cámara de imagen). Al apagar la cámara
con **v**, la captura se detiene y la llamada de audio continúa. Cerrar la
ventana de `mpv` también detiene la cámara y conserva el audio; el panel
muestra el error o cierre de video. El control **v** permite volver a abrirla.

El micrófono se abre después de contestar una entrante o de que el destinatario
acepte una saliente. Colgar, terminar remotamente, perder la conexión o salir
de la TUI cierra los procesos de audio. Mientras haya llamada se bloquea la
grabación de notas de voz; al iniciar/contestar se detiene su reproducción.

## Implementación y dependencias fijadas

- `internal/calls/manager.go`: ciclo de llamada, exclusión de llamadas
  simultáneas, aceptación, rechazo, cancelación, tiempo de espera y limpieza.
- `internal/calls/audio.go`: adaptadores de audio y control de `parec`/`pacat`.
- `internal/calls/video.go` y `h264.go`: cámara, colas de fotogramas, visor,
  orientación y división de tramas H.264.
- `internal/tui/calls.go`: panel, atajos y descripción de estado.
- `meowcaller`: `v0.0.0-20260906234207-c48c3e2a243c`, licencia MIT.
- `hypermeow`: `v0.0.0-20260906031440-90f9e4ec2c88`, variante requerida por
  esa versión de `meowcaller`. Todos los imports del cliente usan la misma
  variante; no se mezclan tipos con `go.mau.fi/whatsmeow`.

`calls.New` se ejecuta antes de `WA.Connect`, porque la biblioteca instala
adaptadores de señalización sobre el cliente. Los avisos y errores de la
biblioteca van a `data/tui.log`. La TUI consulta instantáneas protegidas por
mutex; ningún callback de red espera a que Bubble Tea procese un mensaje.

La sesión SQLite recibe las ampliaciones compatibles v15–v18 de `hypermeow`.
Se comprobó sobre una copia que tanto el binario nuevo como el anterior
pueden leer la sesión resultante y que `PRAGMA integrity_check` devuelve `ok`.

## Verificación

```sh
go test -race -cover ./...
go vet ./...
go build -buildvcs=false -o bin/wa-calls .
```

Las pruebas cubren captura solo después de aceptar, finalización remota,
cancelación durante marcado, resultados tardíos, segunda llamada, errores de
audio, desconexión, silencio, colas acotadas y controles de la TUI.
También cubren división de fotogramas H.264, recuperación tras atraso,
activación y apagado de cámara, llamadas de video entrantes y salientes, y que
un error de video conserve el audio.
Una prueba opcional abre brevemente el micrófono, descarta las muestras y
reproduce silencio, sin llamar ni guardar archivos:

```sh
WA_CALL_AUDIO_SMOKE=1 go test -run TestDesktopAudioSmoke ./internal/calls
```

Esta prueba local de audio pasó el 2026-09-29 en la ThinkPad, al igual que las
pruebas con detector de carreras, `go vet` y la compilación. Binario instalado:
`~/.local/bin/wa-cli.bin`; la versión de video se comprueba con `wa-cli version`.

La prueba opcional de video lee la cámara, codifica H.264, envía las tramas a
`mpv` en modo sin ventana y comprueba que reconoce 640×480. No guarda imágenes
ni llama a nadie:

```sh
WA_CALL_VIDEO_SMOKE=1 go test -run TestDesktopVideoSmoke ./internal/calls
```

Pasó en esta ThinkPad el 2026-09-29. La versión local con video quedó instalada
como `local-video-audio-click-20260929`; también incluye clic izquierdo sobre notas de voz
para reproducirlas, pausarlas y reanudarlas.

La compilación `local-video-viewonce-20260929` evita que ese clic intente abrir
audios de una sola vez: wa-cli muestra que deben abrirse desde el celular. La
misma protección se aplica a la transcripción y a la descarga de medios.

El usuario confirmó el 2026-09-29 el timbrado y el audio en ambos sentidos de
una llamada saliente iniciada con F4 → Enter. Falta confirmar una llamada
entrante contestada desde wa-cli, rechazo, silencio y finalización desde ambos
extremos. Esta evidencia es una prueba manual reportada por el usuario.

Para validar video en vivo, haz una videollamada desde F4 → v y comprueba:
timbrado, imagen local en el teléfono remoto, imagen remota en `mpv`, audio en
ambos sentidos, v apagando/encendiendo cámara y F6 terminando todo. Luego
prueba una videollamada entrante y contéstala con F5. La pila de video de la
biblioteca externa se considera experimental hasta completar esta prueba.

## Respaldo local

Código, binario anterior y copia coherente de `session.db` están en:
`~/.local/share/wa-cli/backups/calls-20260929/` (directorio privado).

Para volver al ejecutable anterior, cierra `wa-cli` y copia `wa-cli.bin` desde
ese respaldo a `~/.local/bin/wa-cli.bin`. Conserva la sesión actual: restaurar
una sesión antigua después de intercambiar mensajes puede perder avances
del estado criptográfico. La copia se conserva para recuperación asistida.

## Referencias

- https://github.com/purpshell/meowcaller
- https://github.com/polymorfa/hypermeow
- https://github.com/tulir/whatsmeow/pull/1201
