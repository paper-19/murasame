#!/usr/bin/env bash
# install.sh: installs murasame as a systemd service on a Linux host, polling one Forgejo instance.
# See README.md next to this file for the prerequisites. Run it as root, with a linux/amd64 (or matching) build:
#
#   sudo FORGEJO_URL=http://forge.example:3000 ./install.sh ./murasame
#
# Optional, to serve the status page beyond the host itself (see "Status page" in README.md):
#   STATUS_ADDR=192.0.2.10:8737             the address the proxy listens on
#   STATUS_URL=http://forge.example:8737    the URL people open; its host name is the only one murasame accepts
#
# Rerun it to upgrade: it replaces the binary, keeps the config and the token, and restarts the service.

set -euo pipefail
if (( EUID != 0 )); then echo "Run it as root (sudo)."; exit 1; fi
if [[ $# -ne 1 || ! -f $1 ]]; then echo "Usage: FORGEJO_URL=<url> $0 <path to the murasame binary>"; exit 1; fi
: "${FORGEJO_URL:?set FORGEJO_URL, e.g. http://forge.example:3000}"
FORGEJO_URL=${FORGEJO_URL%/}
STATUS_ADDR=${STATUS_ADDR:-}
STATUS_URL=${STATUS_URL:-}
if [[ -n $STATUS_ADDR && -z $STATUS_URL ]] || [[ -z $STATUS_ADDR && -n $STATUS_URL ]]; then
  echo "Set both STATUS_ADDR and STATUS_URL, or neither."; exit 1
fi

NEW=$(readlink -f "$1")
USER_NAME=murasame
BIN=/usr/local/bin/murasame
HOME_DIR=/var/lib/murasame
CONF_DIR=/etc/murasame
CONF=$CONF_DIR/murasame.toml
ENV_FILE=$CONF_DIR/env
LISTEN=127.0.0.1:8737
PROXYD=/usr/lib/systemd/systemd-socket-proxyd

command -v git >/dev/null || { echo "Needs git."; exit 1; }
[[ -z $STATUS_ADDR || -x $PROXYD ]] || { echo "Missing $PROXYD (part of systemd), needed for STATUS_ADDR."; exit 1; }
curl -fs -m 5 -o /dev/null "$FORGEJO_URL/api/healthz" || { echo "Forgejo doesn't answer on $FORGEJO_URL."; exit 1; }
"$NEW" --version
cd /

echo
echo "== $USER_NAME system user"
getent passwd "$USER_NAME" >/dev/null ||
  useradd --system --user-group --home-dir "$HOME_DIR" --create-home --shell /usr/sbin/nologin "$USER_NAME"
chmod 0700 "$HOME_DIR"
# murasame commits the merges; git needs an identity for that.
sudo -H -u "$USER_NAME" git config --global user.name murasame
sudo -H -u "$USER_NAME" git config --global user.email murasame@localhost

echo
echo "== Binary $BIN"
install -o root -g root -m 0755 "$NEW" "$BIN.new"
mv -f "$BIN.new" "$BIN"

echo
echo "== Config $CONF"
install -d -o root -g "$USER_NAME" -m 0750 "$CONF_DIR"
if [[ -e $CONF ]]; then
  echo "exists, kept"
else
  read -rp "First repository to queue (owner/name): " repo
  [[ $repo =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]] || { echo "Expected owner/name."; exit 1; }
  read -rp "Forgejo user(s) who may approve, space-separated: " reviewers
  [[ $reviewers =~ ^[A-Za-z0-9._\ -]+$ ]] || { echo "Use Forgejo usernames."; exit 1; }
  list=$(printf '"%s", ' $reviewers); list=${list%, }
  external=
  [[ -n $STATUS_URL ]] && external="external_url = \"$STATUS_URL\""
  install -m 0640 -o root -g "$USER_NAME" /dev/null "$CONF"
  cat >"$CONF" <<EOF
# murasame. After a change: systemctl restart murasame
# Check it with: sudo -u $USER_NAME murasame check-config --config $CONF

[instance]
bot_user = "murasame"
state_dir = "$HOME_DIR"
listen = "$LISTEN"
$external

[forges.main]
url = "$FORGEJO_URL"
token_env = "MURASAME_FORGEJO_TOKEN"

# One [[repos]] block per repository. Its gate runs in the merged tree; exit 0 = pass.
[[repos]]
repository = "$repo"

[repos.repo]
target = ["main"]

[repos.gates.check]
command = ["make", "check"]
timeout = 3600

[repos.forgejo]
reviewers = [$list]
EOF
  echo "Opening it so you can set the gate command (and the target branch, if not main)."
  read -rp "Press Enter to edit. "
  "${EDITOR:-vi}" "$CONF"
fi
sudo -u "$USER_NAME" "$BIN" check-config --config "$CONF"

echo
echo "== Token $ENV_FILE"
if [[ -s $ENV_FILE ]]; then
  echo "exists, kept"
else
  read -rsp "Forgejo access token of the murasame user (not shown): " token; echo
  [[ $token =~ ^[A-Za-z0-9]+$ ]] || { echo "That doesn't look like a Forgejo token."; exit 1; }
  install -m 0640 -o root -g "$USER_NAME" /dev/null "$ENV_FILE"
  printf 'MURASAME_FORGEJO_TOKEN=%s\n' "$token" >"$ENV_FILE"
  unset token
fi
# The token must belong to the bot user: murasame comments as it and pushes the target branches with it.
# The header goes through stdin (-H @-), so the token never shows in ps.
reply=$(. "$ENV_FILE"; printf 'Authorization: token %s\n' "$MURASAME_FORGEJO_TOKEN" |
  curl -sS -m 5 -H @- "$FORGEJO_URL/api/v1/user") || true
login=$(sed -n 's/.*"login":"\([^"]*\)".*/\1/p' <<<"$reply")
if [[ $login == murasame ]]; then echo "  ok    the token signs in as murasame"; else
  echo "  FAIL  the token signs in as '${login:-nothing}', not murasame. Forgejo says: $reply"
  echo "        'You must change your password': forgejo admin user must-change-password --unset murasame"
  echo "        A wrong token: edit $ENV_FILE, then rerun."
  exit 1
fi

echo
echo "== systemd units"
cat >/etc/systemd/system/murasame.service <<EOF
[Unit]
Description=murasame merge queue for Forgejo
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
User=$USER_NAME
Group=$USER_NAME
WorkingDirectory=$HOME_DIR
Environment=HOME=$HOME_DIR
EnvironmentFile=$ENV_FILE
ExecStart=$BIN serve --config $CONF
Restart=always
RestartSec=10
# A running gate gets time to stop.
TimeoutStopSec=60
UMask=0077
NoNewPrivileges=yes
PrivateTmp=yes
# Gates can't see /home: install their toolchains system-wide (or drop this line).
ProtectHome=yes

[Install]
WantedBy=multi-user.target
EOF
units=(murasame)
if [[ -n $STATUS_ADDR ]]; then
  # murasame serves its status page to loopback clients only; the proxy makes remote requests arrive from loopback.
  cat >/etc/systemd/system/murasame-proxy.socket <<EOF
[Unit]
Description=murasame status page on $STATUS_ADDR

[Socket]
ListenStream=$STATUS_ADDR
# Binds even before the address exists (e.g. a VPN interface that comes up late).
FreeBind=yes

[Install]
WantedBy=sockets.target
EOF
  cat >/etc/systemd/system/murasame-proxy.service <<EOF
[Unit]
Description=murasame status page proxy: $STATUS_ADDR -> $LISTEN
Requires=murasame-proxy.socket
After=murasame-proxy.socket murasame.service

[Service]
ExecStart=$PROXYD $LISTEN
DynamicUser=yes
PrivateTmp=yes
NoNewPrivileges=yes
EOF
  units+=(murasame-proxy.socket)
fi
systemctl daemon-reload
systemctl enable "${units[@]}"
systemctl restart murasame
if [[ -n $STATUS_ADDR ]]; then
  systemctl stop murasame-proxy.service 2>/dev/null || true
  systemctl restart murasame-proxy.socket
fi

up=
for _ in $(seq 24); do
  curl -fs -m 3 -o /dev/null "http://$LISTEN/healthz" && { up=1; break; }
  sleep 5
done
if [[ $up ]]; then echo "  ok    murasame answers on $LISTEN"; else
  echo "  FAIL  murasame didn't come up in 2 minutes: journalctl -u murasame -n 50"
  exit 1
fi
if [[ -n $STATUS_ADDR ]]; then
  # Ask the way a browser would: at STATUS_ADDR, with STATUS_URL's host name.
  hostport=${STATUS_URL#*://}; hostport=${hostport%%/*}
  host=${hostport%:*}; port=${STATUS_ADDR##*:}; ip=${STATUS_ADDR%:*}
  code=$(curl -s -o /dev/null -m 5 -w '%{http_code}' --resolve "$host:$port:$ip" "http://$host:$port/" || true)
  if [[ $code == 200 ]]; then echo "  ok    status page answers on $STATUS_URL"; else
    echo "  FAIL  $STATUS_URL returned ${code:-nothing}, expected 200: journalctl -u murasame-proxy -u murasame -n 50"
    exit 1
  fi
fi

cat <<EOF

murasame $("$BIN" --version | awk '{ print $2 }') is running.
Status:   ${STATUS_URL:-http://$LISTEN (on this host only)}
Try it:   comment "@murasame ping" on a pull request
Config:   $CONF, then systemctl restart murasame
Logs:     journalctl -u murasame -f
EOF
