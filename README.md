# wa-cli

> Hola, ¿cómo están? Bueno, este proyecto realmente fue una mezcla entre el backend que encontré y un frontend go que encontré. Entonces, espero que les guste mi adaptación. Los verdaderos repos van a estar aquí abajo del README.

> **Fork** de [codebyoketch/wa-cli](https://github.com/codebyoketch/wa-cli) (MIT) con mejoras propias: notas de voz y transcripción, llamadas, vistos/presencia, selección múltiple, menú contextual, arrastrar archivos, notificaciones y más.
>
> **Backend:** no se copia código de WhatsApp. Se usa como dependencia [`polymorfa/hypermeow`](https://github.com/polymorfa/hypermeow) (variante de [whatsmeow](https://github.com/tulir/whatsmeow)) y [`meowcaller`](https://github.com/purpshell/meowcaller). Créditos a sus autores.
>
> **Seguir el backend:** Dependabot (`.github/dependabot.yml`) abre un PR cada vez que hypermeow sube de versión. A mano:
> `go get github.com/polymorfa/hypermeow@latest && go mod tidy && go build ./...`
>
> **Privacidad:** el repo no incluye sesión, mensajes ni claves. Cada persona hace `wa-cli login` y escanea su propio QR. Datos en `~/.config/wa/data/`.

A WhatsApp client for your terminal, built in Go on top of the
[hypermeow](https://github.com/polymorfa/hypermeow) variant of whatsmeow.
This local version integrates experimental Linux audio and video calls with
[meowcaller](https://github.com/purpshell/meowcaller): **F4** opens the call
panel, **Enter** starts audio, **v** starts video, **F5** answers, and **F6**
hangs up. Remote video opens in `mpv`. See [calls](docs/CALLS.md) for setup and
validation. Outgoing two-way audio was confirmed; video still needs a live call.

> **Status:** v1.0.0 — stable and cross-platform, with a couple of
> documented known limitations rather than zero known bugs; see
> [ROADMAP.md](./ROADMAP.md) for exactly what's done and what's known
> to still need work.

> **Docs:** browse this repo's docs as a site at
> [codebyoketch.github.io/wa-cli](https://codebyoketch.github.io/wa-cli/),
> or generated API docs at
> [pkg.go.dev/github.com/codebyoketch/wa-cli](https://pkg.go.dev/github.com/codebyoketch/wa-cli).

## Why

Scriptable, fast, terminal-first WhatsApp: send and receive messages,
manage chats/contacts/groups/media, all from the command line — with
`--json` output for scripting and automation, and a full-screen TUI
(`wa`, no subcommand) for everyday use.

## Install

**Prebuilt binaries** (Linux/macOS/Windows, amd64/arm64) are on the
[Releases page](https://github.com/codebyoketch/wa-cli/releases).
Download the archive for your platform, extract it, and put the `wa`
binary somewhere on your `PATH`.

```sh
# Homebrew (macOS/Linux)
brew install codebyoketch/tap/wa-cli

# Scoop (Windows)
scoop bucket add wa-cli https://github.com/codebyoketch/scoop-bucket
scoop install wa-cli

# Docker
docker run -it -v wa-data:/data ghcr.io/codebyoketch/wa-cli:latest login
```

> **Homebrew on Linux:** if you don't already have `brew`, it isn't
> preinstalled — get it from [brew.sh](https://brew.sh) first
> (`/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"`).
> The installer prints a "Next steps" section with a `PATH` line you
> need to run before `brew` works in your current shell — usually
> `eval "$(/home/linuxbrew/.linuxbrew/bin/brew shellenv bash)"`, added
> to `~/.bashrc` (or your shell's equivalent) so it persists across
> terminals. It also asks for `build-essential` and `bubblewrap` via
> `apt`, needed to install some formulae on Linux.

Or with Go 1.25+ installed:

```sh
go install github.com/codebyoketch/wa-cli@latest
```

Or build from source:

```sh
git clone https://github.com/codebyoketch/wa-cli.git
cd wa-cli
make build   # binary at ./bin/wa
```

For quick local iteration without a build step, use `go run .` in place
of `wa` anywhere below.

## Getting started

```sh
wa login              # scan the QR code with WhatsApp on your phone
wa status             # confirm you're logged in
wa chat list           # sync and list your chats
```

Run `wa` with no subcommand to open the full-screen terminal UI instead
of using individual commands.

## Usage

**Chats**
```sh
wa chat list [--no-sync]        # list chats, most recent first
wa chat search <query>          # search chats by name
wa chat info <jid-or-name>      # details for one chat
wa chat open <jid-or-name>      # show local message history
wa chat send <recipient> <message...>
wa chat reply <recipient> <message-ref> <message...>
wa chat forward <from-chat> <message-ref> <to-recipient>
wa chat mute / unmute <chat>    # suppress desktop notifications per chat
```

In the full-screen UI, focus the chat list and press `f` to forward: choose a
retained message with `↑`/`↓`, press `Enter`, choose the destination chat, and
confirm. Re-forwarding requires the original WhatsApp payload kept in local
history, so very old entries without it cannot be sent.

**Receiving**
```sh
wa watch                        # long-running: print incoming messages live
```

**Contacts**
```sh
wa contact list
wa contact search <query>
wa contact info <name-or-jid>
```

**Groups**
```sh
wa group list
wa group info <name-or-jid>
wa group create <name> <participant...>
wa group add / remove <group> <participant...>
```

**Media**
```sh
wa media send image/video/audio/document/sticker <recipient> <file>
wa media download <chat> <message-ref> [output-path]
wa media list <chat>            # media messages in a chat's local history
```
`image` send/download are verified against a real account; the other
media types are implemented but not yet confirmed end-to-end — see
Phase 8 in [ROADMAP.md](./ROADMAP.md).

**Config**
```sh
wa config get [key]             # print all settings, or one
wa config set <key> <value>     # change and save one setting
wa config edit                  # open config.json in $EDITOR
wa config init                  # write out defaults
```

**Extensions**
```sh
wa extension install <git-url>
wa extension list
wa extension run <name> [-- args...]
wa extension remove <name>
```

**Other**
```sh
wa status                       # login status
wa version
wa completion bash|zsh|fish|powershell
```

### JSON output

Every list/read command above (`chat list/search/info/open`, `contact
list/search/info`, `group list/info`, `media list`, `status`, `extension
list`) supports `--json` for scripting:

```sh
wa chat list --json | jq '.[] | .name'
```

Set it as the default instead of passing the flag every time:

```sh
wa config set jsonOutput true
```

### Notifications

Desktop notifications fire from `wa watch` and the TUI when a new message
arrives (not from one-shot commands). Configurable via `wa config set`:

```sh
wa config set notifyEnabled true|false
wa config set notifyGroups true|false        # group chats opt-in separately from DMs
wa config set notifyShowPreview true|false   # include message text in the notification
wa chat mute <chat>                          # suppress notifications for one chat
```

## Development

```sh
make build   # compile ./bin/wa
make test    # go test ./...
make lint    # golangci-lint run
make fmt     # gofmt -w .
```

## Roadmap

See [ROADMAP.md](./ROADMAP.md) for the full phase-by-phase plan, what's
done, what's in progress, and known issues.

## Architecture

See [ARCHITECTURE.md](./ARCHITECTURE.md) for a map of the codebase —
package layout and how the pieces fit together.

For local data, performance behaviour, history migration, keyboard shortcuts,
and safe day-to-day operation, see [docs/OPERATIONS.md](./docs/OPERATIONS.md).
The current technical audit is recorded in [docs/AUDIT.md](./docs/AUDIT.md).
The terminal layout fix and its verification are recorded in
[docs/TUI_LAYOUT_2026-09-28.md](./docs/TUI_LAYOUT_2026-09-28.md).

## Examples

See [docs/EXAMPLES.md](./docs/EXAMPLES.md) for worked examples: scripting
with `--json`, a daily unread-chats digest, piping `wa watch`, sending
from a script without hitting an interactive prompt, exporting chat
history, and writing a minimal extension.

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md).

## Security

See [SECURITY.md](./SECURITY.md) for how to report vulnerabilities.

## License

[MIT](./LICENSE)

## Repos originales

Los verdaderos autores de las piezas que componen este proyecto:

| Pieza | Repo |
|---|---|
| Frontend (TUI/CLI) original | [codebyoketch/wa-cli](https://github.com/codebyoketch/wa-cli) |
| Backend de WhatsApp (variante) | [polymorfa/hypermeow](https://github.com/polymorfa/hypermeow) |
| Backend base | [tulir/whatsmeow](https://github.com/tulir/whatsmeow) |
| Llamadas de audio/video | [purpshell/meowcaller](https://github.com/purpshell/meowcaller) |
