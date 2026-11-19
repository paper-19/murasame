# Running murasame as a systemd service

This example installs murasame on a Linux host as a service that polls one Forgejo
instance. [`install.sh`](install.sh) does the work. This page lists what to do before
and after you run it.

What you get:

| | |
|---|---|
| Binary | `/usr/local/bin/murasame` |
| User | `murasame` system user, home `/var/lib/murasame` (mode 0700): clones, integration trees, gate logs, SQLite DB |
| Config | `/etc/murasame/murasame.toml` (`root:murasame`, 0640) |
| Token | `/etc/murasame/env` (`root:murasame`, 0640) |
| Units | `murasame.service`, plus `murasame-proxy.socket`/`.service` if the status page is served beyond the host |
| Logs | `journalctl -u murasame` (murasame redacts the token) |

murasame polls Forgejo every 60 seconds, so you don't need a webhook.

## Prerequisites

### On the host

- Linux with systemd, `git` and `curl`.
- A network path to Forgejo's HTTP address. murasame talks to the API and pushes over
  HTTP with the token.
- **The tools your gates need, installed system-wide.** Gates run as the `murasame`
  user, and the unit hides `/home` (`ProtectHome=yes`), so a toolchain under someone's
  home directory isn't visible. For a Go repository, for example: `apt install golang-go`.
  An older Go downloads the toolchain that `go.mod` asks for by itself.

### A murasame binary

Build it on any machine with Go, for the host's platform:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o murasame ./cmd/murasame
scp murasame install.sh you@host:/tmp/
```

The SQLite driver is pure Go, so no cgo or C cross-compiler is needed.

### In Forgejo

1. **Create the bot user and a token**, as Forgejo's admin on the Forgejo host. Forgejo's
   CLI marks new users "must change password" by default. Turn that off, or every API
   call from the bot gets a 403:

   ```sh
   forgejo admin user create --username murasame --email murasame@localhost \
     --random-password --must-change-password=false
   forgejo admin user generate-access-token --username murasame --token-name murasame --raw \
     --scopes write:repository,write:issue,read:user
   ```

   Run these as Forgejo's system user, with its `--config`/`--work-path`. The token is
   printed once. Keep it for the install.
2. **Give `murasame` Write access** to each repository you'll queue (Settings →
   Collaborators).
3. **Have the target branch** (`main` unless you change it) in the repository.

## Install

```sh
sudo FORGEJO_URL=http://forge.example:3000 /tmp/install.sh /tmp/murasame
```

The script:

1. Creates the `murasame` user and gives it a git identity for merge commits.
2. Installs the binary.
3. On the first run, asks for the repository (`owner/name`) and who may approve, writes
   the config, and opens it in `$EDITOR`. Set the gate command there, for example:

   ```toml
   [repos.gates.check]
   command = ["sh", "-c", "go vet ./... && go build ./..."]
   ```

4. On the first run, asks for the token and checks that it signs in as `murasame`.
5. Installs and starts the units, then checks `/healthz` and the status page.

Rerun it with a new binary to upgrade. It keeps the config and the token.

## Status page

murasame serves its status page (`/`, `/api/status`, gate logs) **only to clients on the
same host**, and only when they ask for a loopback name or the host in `external_url`.
The page has no login. By default the install leaves it on `127.0.0.1:8737`. To reach it
from elsewhere, either tunnel it:

```sh
ssh -N -L 8737:127.0.0.1:8737 you@host     # then open http://localhost:8737
```

or let the script add a small proxy (`systemd-socket-proxyd`). The proxy makes requests
from your network arrive at murasame from loopback:

```sh
sudo FORGEJO_URL=http://forge.example:3000 \
  STATUS_ADDR=192.0.2.10:8737 STATUS_URL=http://forge.example:8737 \
  /tmp/install.sh /tmp/murasame
```

- `STATUS_ADDR` is the address and port the proxy listens on. Use a private interface
  (a LAN or VPN address), not a public one: anyone who reaches it can read the queue and
  the gate logs.
- `STATUS_URL` becomes `external_url`. Open the page by **that host name**. murasame
  answers other names, the raw IP included, with 403.

On an existing install, also set `listen = "127.0.0.1:8737"` and the `external_url` in
the config. The script only writes the config on the first run.

## After installing

- **Protect the target branch** (Settings → Branches) in each repository, so nothing
  merges around the queue:
  - **Push:** restricted to an allowlist containing only `murasame`.
  - **Merge:** allowlist only `murasame`. This hides the merge button from everyone else.
  - **Approvals:** leave at 0. Approval is the `@murasame approve <sha>` comment.
- **Try it:** comment `@murasame ping` on a pull request. The reply comes within a minute.

## Security notes

- **Gates run as `murasame`**, the same user that holds the token, so gate commands can
  read it. murasame warns about this at startup. That's fine for repositories whose code
  you trust. Otherwise set `[repos.repo] command_prefix` to run gates as another user or
  in a container.
- The token and the state directory are readable only by root and `murasame`.

## Removing it

```sh
sudo systemctl disable --now murasame murasame-proxy.socket
sudo rm /etc/systemd/system/murasame{.service,-proxy.socket,-proxy.service}
sudo rm -r /etc/murasame /usr/local/bin/murasame
sudo userdel -r murasame          # also deletes /var/lib/murasame
```
