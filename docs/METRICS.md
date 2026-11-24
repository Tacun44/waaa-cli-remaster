# Métricas locales del TUI

El TUI guarda eventos de uso del propietario en `<DataDir>/metrics.db` (por defecto `~/.config/wa/data/metrics.db`). `WA_METRICS=0` desactiva el grabador y evita crear el archivo. No se almacena el texto de mensajes ni transcripciones. El escritor usa WAL, un canal de 1024 eventos y lotes de hasta 100 eventos o 2 segundos; si el canal se llena, se descartan eventos. Al salir se vacía el canal y se escribe `session_end`.

## Esquema

```sql
CREATE TABLE chats (id INTEGER PRIMARY KEY, jid TEXT NOT NULL UNIQUE);
CREATE TABLE events (ts INTEGER NOT NULL, kind INTEGER NOT NULL, chat INTEGER, msg TEXT, a INTEGER, b INTEGER);
CREATE INDEX events_ts ON events(ts);
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
```

`ts` es Unix en milisegundos; `chat` referencia `chats.id`; `msg` es el ID de WhatsApp. `meta` incluye `schema_version=1` y la descripción del enum en `kinds`.

| kind | Evento | a | b |
|---:|---|---|---|
| 1 | session_start | PID | |
| 2 | session_end | | |
| 3, 4 | focus, blur | | |
| 5, 6 | idle_start, idle_end | | |
| 10 | chat_open | no leídos | |
| 20, 21, 22 | image_open, video_open, document_open | timestamp del mensaje (ms) | enviado por mí: 1/0 |
| 30 | audio_play_start | duración del audio (s) | timestamp del mensaje (ms) |
| 31 | audio_play_end | escucha (ms) | completado: 1/0 |
| 32 | transcript_shown | | |
| 40 | compose_start | | |
| 41 | send | composición (ms) | caracteres |
| 42 | compose_abandon | composición (ms) | caracteres |
| 43 | dictation_used | duración del dictado (ms; 0 si se desconoce) | caracteres |
| 44 | media_send | 1 imagen, 2 video, 3 audio, 4 sticker, 5 documento | |
| 45 | reply_quote | | |
| 46 | compose_keys | teclas | retrocesos |

`idle_start` usa la hora de la última entrada tras 120 segundos sin teclas ni ratón mientras la terminal tiene foco. El tick de presencia existente realiza la comprobación cada 5 segundos. `compose_keys` se registra justo antes de `send`. Un `send` solo se registra si WhatsApp confirma el envío.
