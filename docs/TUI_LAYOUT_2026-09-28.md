# Descuadre de la TUI al dibujar chats — 2026-09-28

## Síntomas

En una terminal ancha se observaron mensajes y separadores de fecha fuera de
posición, la barra de escritura ausente o desplazada, la barra lateral cortada
y espacio vacío debajo de la interfaz. En otra captura, el nombre de un chat
aparecía tanto en la primera fila de la barra lateral como en su fila de la
lista. La primera fila debía ser el encabezado de la lista. La caché local
contenía una sola entrada para ese nombre: la duplicación visible no indicaba
un segundo chat guardado.

## Diagnóstico

- `model.View()` anteponía `ESC[1;1H` al cuadro. Bubble Tea ya posiciona el
  cursor al pintar la pantalla alternativa. El movimiento adicional no debe
  formar parte del contenido que su renderizador compara y actualiza.
- La línea de atajos tenía 214 columnas en una terminal de 133. Como
  `lipgloss.JoinVertical` usa el ancho del bloque más largo, ampliaba también
  todas las filas del cuerpo a 214 columnas. Bubble Tea debía truncarlas al
  final de su propio renderizado; ese desborde hacía frágil la disposición.
- El tamaño del modelo dependía del evento de redimensionado de Bubble Tea.
  Si la terminal cambiaba de tamaño sin que llegara el evento, el marco seguía
  con las medidas anteriores y podía terminar varias filas antes del borde.

La captura por sí sola no permite atribuir cada píxel fuera de lugar a una
única de estas causas. Las tres condiciones sí estaban presentes en el código
y se corrigieron juntas. No se modificaron mensajes ni datos de sesión.

## Corrección

En `internal/tui/tui.go`:

1. `View()` devuelve el cuadro escaneado por `bubblezone` sin una secuencia de
   posicionamiento de cursor; Bubble Tea conserva el control del cursor.
2. Las líneas de estado y atajos se truncan con `ansi.Truncate` al ancho real
   `m.width` antes de unirse al cuerpo. Así el pie no ensancha los paneles.
3. La barra lateral muestra el título fijo `Chats`, distinguible de las filas
   de conversaciones.
4. El ciclo de presencia de cinco segundos solicita además `tea.WindowSize()`.
   Un `WindowSizeMsg` con dimensiones nuevas recalcula el `viewport` y limpia
   la pantalla; uno con dimensiones idénticas se ignora para evitar parpadeos.

## Verificación

Se añadieron pruebas en `internal/tui/tui_test.go` que comprueban que:

- el cuadro no contiene `ESC[1;1H` y conserva la caja de escritura;
- con barra lateral, mensajes largos y tres días de historial, cada línea
  ocupa como máximo el ancho de la terminal y el cuadro usa su altura;
- el encabezado `Chats` aparece en la barra lateral;
- una medida igual no fuerza limpieza, y una medida nueva actualiza el layout.

`go test ./...` pasó. Se compiló con `go build -buildvcs=false` porque esta
copia fuente no tiene metadatos Git. El ejecutable actualizado se instaló en
`~/.local/bin/wa-cli.bin`, que invoca el lanzador `~/.local/bin/wa-cli`.
Se guardó el binario anterior en
`~/.local/bin/wa-cli.bin.backup-before-visual-fix-20260928`.

La validación automática usa un modelo de TUI con datos de prueba; no captura
una sesión real conectada a WhatsApp. Una sesión abierta antes de instalar el
binario conserva el código anterior hasta cerrarla y abrirla de nuevo.

## Si reaparece

1. Reiniciar `wa-cli` y comprobar que el título lateral dice `Chats`.
2. Cambiar el tamaño de la terminal y esperar hasta cinco segundos. El borde
   lateral y la caja de escritura deben quedar dentro del marco.
3. Ejecutar `go test ./internal/tui` en la copia fuente.
4. Si persiste, guardar una captura con el tamaño de terminal en filas y
   columnas; revisar primero `View()`, `layout()`, los pies y las escrituras
   directas al terminal. Evitar compartir `tui.log` o la caché sin revisar su
   contenido personal.
