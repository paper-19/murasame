#!/usr/bin/env bash
# End-to-end check against a real Forgejo with Actions, in Docker. Not part of
# `go test`: it pulls images and takes a few minutes.
#
#   tests/e2e/forgejo.sh            # Forgejo 15 and runner 9
#   FORGEJO_IMAGE=codeberg.org/forgejo/forgejo:16 tests/e2e/forgejo.sh
#
# It starts Forgejo and a runner on a private Docker network (Forgejo on
# 127.0.0.1:$PORT), creates a repository with a two-job workflow on the staging
# branch, opens PRs, runs `murasame tick`, and checks:
#   1. a batch with a broken PR is bisected: the good PR lands, the broken one is set aside;
#   2. a required job skipped because the job it needs failed sets the PR aside;
#   3. `@murasame retry` on the same commit starts a new run, ignores the old statuses and lands;
#   4. landed PRs are marked merged;
#   5. `murasame serve` with a one-hour poll lands a PR within a minute: Forgejo's
#      webhooks wake it;
#   6. with `contexts = []`, a failing status the PR's own pull_request run left on
#      the commit does not decide: only the run murasame's push started does;
#   7. one `murasame serve` with a multi-repo config lands PRs in two repositories;
#   8. a push to the staging branch by hand cancels murasame's run: the PR goes back
#      to the queue instead of being set aside;
#   9. with the runner stopped, the gate times out and says the jobs never started.
# Everything is removed at the end (KEEP=1 keeps the containers).
set -euo pipefail

FORGEJO_IMAGE=${FORGEJO_IMAGE:-codeberg.org/forgejo/forgejo:15}
RUNNER_IMAGE=${RUNNER_IMAGE:-code.forgejo.org/forgejo/runner:9}
PORT=${PORT:-3333}
SERVE_PORT=${SERVE_PORT:-8737}
NET=murasame-e2e
FJ=murasame-e2e-forgejo
RUN=murasame-e2e-runner
API=http://127.0.0.1:$PORT/api/v1
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
WORK=$(mktemp -d)

cleanup() {
  set +e
  [[ -n ${SERVE_PID:-} ]] && kill "$SERVE_PID" 2>/dev/null
  if [[ ${KEEP:-} != 1 ]]; then
    docker rm -f "$RUN" "$FJ" >/dev/null 2>&1
    docker network rm "$NET" >/dev/null 2>&1 || true
    rm -rf "$WORK"
  else
    echo "kept: containers $FJ $RUN, files $WORK"
  fi
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
api() { local tok=$1; shift; curl -sf -H "Authorization: token $tok" -H 'Content-Type: application/json' "$@"; }
json() { python3 -c "import sys,json; d=json.load(sys.stdin); print($1)"; }

echo "== Forgejo ($FORGEJO_IMAGE) and runner ($RUNNER_IMAGE)"
docker rm -f "$RUN" "$FJ" >/dev/null 2>&1 || true
docker network rm "$NET" >/dev/null 2>&1 || true
docker network create "$NET" >/dev/null
docker run -d --name "$FJ" --network "$NET" -p "127.0.0.1:$PORT:3000" --add-host host.docker.internal:host-gateway \
  -e FORGEJO__webhook__ALLOWED_HOST_LIST='*' \
  -e FORGEJO__security__INSTALL_LOCK=true -e FORGEJO__database__DB_TYPE=sqlite3 \
  -e FORGEJO__server__ROOT_URL="http://$FJ:3000/" -e FORGEJO__actions__ENABLED=true \
  -e FORGEJO__service__DISABLE_REGISTRATION=true -e FORGEJO__repository__DEFAULT_BRANCH=main \
  "$FORGEJO_IMAGE" >/dev/null
for _ in $(seq 1 90); do curl -sf "$API/version" >/dev/null && break; sleep 1; done
curl -sf "$API/version" >/dev/null || fail "Forgejo did not start"

fjcli() { docker exec -u git "$FJ" forgejo "$@"; }
for u in root murasame alice; do
  admin=(); [[ $u == root ]] && admin=(--admin)
  fjcli admin user create "${admin[@]}" --username "$u" --password "Pass-$u-12345" --email "$u@example.com" \
    --must-change-password=false >/dev/null
done
R=$(fjcli admin user generate-access-token --username root --token-name t --raw --scopes all)
B=$(fjcli admin user generate-access-token --username murasame --token-name t --raw \
  --scopes write:repository,write:issue,read:user)
A=$(fjcli admin user generate-access-token --username alice --token-name t --raw \
  --scopes write:repository,write:issue,read:user)
token=$(fjcli actions generate-runner-token | tail -1)
docker run -d --name "$RUN" --network "$NET" --entrypoint sh "$RUNNER_IMAGE" -c \
  "cd /tmp && forgejo-runner register --no-interactive --instance http://$FJ:3000 --token $token --name e2e \
   --labels host:host && forgejo-runner daemon" >/dev/null

echo "== repository root/app with a workflow on murasame/staging"
api "$R" -d '{"name":"app","auto_init":true,"default_branch":"main"}' "$API/user/repos" >/dev/null
for u in murasame alice; do api "$R" -X PUT -d '{"permission":"write"}' "$API/repos/root/app/collaborators/$u"; done
api "$R" -X PATCH -d '{"allow_manual_merge":true}' "$API/repos/root/app" >/dev/null
cat >"$WORK/ci.yml" <<'EOF'
name: ci
on:
  push:
    branches: [murasame/staging]
  pull_request:
concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: true
jobs:
  build:
    runs-on: host
    steps:
      - run: |
          git clone -q "http://ci:${TOKEN}@${HOST}/${REPO}.git" w && cd w && git checkout -q "$SHA"
          if [ "$EVENT" = push ] && [ -e FLAKY ] && [ ! -e "/tmp/flaky-$(cat FLAKY)" ]; then
            touch "/tmp/flaky-$(cat FLAKY)"; echo "flaky"; exit 1
          fi
        env: { TOKEN: "${{ github.token }}", REPO: "${{ github.repository }}", SHA: "${{ github.sha }}", HOST: "HOSTPORT",
               EVENT: "${{ github.event_name }}" }
  test:
    needs: build
    runs-on: host
    steps:
      - run: |
          git clone -q "http://ci:${TOKEN}@${HOST}/${REPO}.git" w && cd w && git checkout -q "$SHA"
          if [ -e SLOW ]; then sleep 30; fi
          if [ -e BROKEN ]; then echo "BROKEN is present"; exit 1; fi
        env: { TOKEN: "${{ github.token }}", REPO: "${{ github.repository }}", SHA: "${{ github.sha }}", HOST: "HOSTPORT" }
  lint:
    if: github.event_name == 'pull_request'
    runs-on: host
    steps:
      - run: echo "lint fails on every PR"; exit 1
EOF
sed -i.bak "s/HOSTPORT/$FJ:3000/" "$WORK/ci.yml"
python3 - "$WORK/ci.yml" >"$WORK/body.json" <<'EOF'
import sys, json, base64
print(json.dumps({"content": base64.b64encode(open(sys.argv[1], "rb").read()).decode(), "message": "CI", "branch": "main"}))
EOF
api "$R" -d @"$WORK/body.json" "$API/repos/root/app/contents/.forgejo/workflows/ci.yml" >/dev/null

git clone -q "http://root:$R@127.0.0.1:$PORT/root/app.git" "$WORK/src"
git -C "$WORK/src" config user.name root
git -C "$WORK/src" config user.email root@example.com
pr() { # pr BRANCH FILE: a branch off main adding FILE, opened as a PR; prints "number sha"
  git -C "$WORK/src" fetch -q origin
  git -C "$WORK/src" checkout -q -B "$1" origin/main
  echo "$1" >"$WORK/src/$2"
  git -C "$WORK/src" add -A && git -C "$WORK/src" commit -q -m "$1"
  git -C "$WORK/src" push -q -f origin "$1" 2>/dev/null
  api "$R" -d "{\"head\":\"$1\",\"base\":\"main\",\"title\":\"$1\"}" "$API/repos/root/app/pulls" |
    json 'd["number"], d["head"]["sha"]' | tr -d "(),'"
}
say() { api "$A" -d "{\"body\":\"$2\"}" "$API/repos/root/app/issues/$1/comments" >/dev/null; }
merged() { api "$R" "$API/repos/root/app/pulls/$1" | json 'd["merged"]'; }

echo "== murasame"
go build -C "$ROOT" -o "$WORK/murasame" ./cmd/murasame
cat >"$WORK/m.toml" <<EOF
[ci]
contexts = ["ci / test (push)"]
timeout = 600
start_timeout = 180
poll_interval = 2

[forgejo]
url = "http://127.0.0.1:$PORT"
owner = "root"
repo = "app"
git_remote_url = "http://127.0.0.1:$PORT/root/app.git"
reviewers = ["alice"]
infra_backoff = 1
EOF
export GIT_AUTHOR_NAME=murasame GIT_AUTHOR_EMAIL=murasame@example.com
export GIT_COMMITTER_NAME=murasame GIT_COMMITTER_EMAIL=murasame@example.com
echo "== 0: check-config --forge"
MURASAME_FORGEJO_TOKEN=$B "$WORK/murasame" check-config --forge --config "$WORK/m.toml" >"$WORK/check.log" 2>&1 ||
  fail "check-config --forge failed: $(grep -E 'FAIL|WARN' "$WORK/check.log")"
grep -q "mention(s) murasame/staging" "$WORK/check.log" || fail "check-config --forge did not find the workflow"
grep -q "WARN" "$WORK/check.log" && fail "check-config --forge warned: $(grep WARN "$WORK/check.log")"
tick() { MURASAME_FORGEJO_TOKEN=$B "$WORK/murasame" tick --config "$WORK/m.toml" --repo "$WORK/clone" >>"$WORK/tick.log" 2>&1; }

echo "== 1: bisection"
read -r good good_sha < <(pr good a.txt)
read -r bad bad_sha < <(pr bad BROKEN)
say "$good" "@murasame approve $good_sha"
say "$bad" "@murasame approve $bad_sha"
tick
[[ $(merged "$good") == True ]] || fail "the good PR #$good did not land (see $WORK/tick.log)"
[[ $(merged "$bad") == False ]] || fail "the broken PR #$bad landed"
grep -q "SET ASIDE #$bad" "$WORK/tick.log" || fail "the broken PR #$bad was not set aside"

echo "== 2 and 3: a skipped required job, then a retry of the same commit"
read -r flaky flaky_sha < <(pr flaky FLAKY)
say "$flaky" "@murasame approve $flaky_sha"
tick
[[ $(merged "$flaky") == False ]] || fail "#$flaky landed although its required job was skipped"
grep -q "a required job was skipped after ci / build (push): failure" "$WORK/tick.log" ||
  fail "the skipped required job was not reported"
say "$flaky" "@murasame retry"
tick
[[ $(merged "$flaky") == True ]] || fail "the retry of #$flaky did not land (see $WORK/tick.log)"
grep -q "already has CI statuses" "$WORK/tick.log" || fail "the retry did not see the old statuses"

echo "== 4: marked merged by the bot"
by=$(api "$R" "$API/repos/root/app/pulls/$good" | json '(d.get("merged_by") or {}).get("login")')
[[ $by == murasame ]] || fail "#$good was marked merged by '$by', not murasame"

echo "== 5: serve, woken by webhooks"
sed -i.bak 's/^reviewers = .*/&\npoll_interval = 3600\nlisten = "0.0.0.0:'"$SERVE_PORT"'"/' "$WORK/m.toml"
MURASAME_FORGEJO_TOKEN=$B MURASAME_WEBHOOK_SECRET=e2e "$WORK/murasame" serve --config "$WORK/m.toml" \
  --repo "$WORK/clone" >"$WORK/serve.log" 2>&1 &
SERVE_PID=$!
api "$R" -d '{"type":"forgejo","active":true,"events":["push","issue_comment","pull_request"],"config":{"url":
  "http://host.docker.internal:'"$SERVE_PORT"'/webhook","content_type":"json","secret":"e2e"}}' \
  "$API/repos/root/app/hooks" >/dev/null
sleep 3
kill -0 "$SERVE_PID" 2>/dev/null || fail "serve exited: $(tail -1 "$WORK/serve.log")"
read -r hooked hooked_sha < <(pr hooked b.txt)
say "$hooked" "@murasame approve $hooked_sha"
for _ in $(seq 1 60); do [[ $(merged "$hooked") == True ]] && break; sleep 1; done
[[ $(merged "$hooked") == True ]] || fail "serve did not land #$hooked within 60 s (see $WORK/serve.log)"
grep -q 'POST /webhook HTTP/1.1" 202' "$WORK/serve.log" || fail "no webhook reached serve"
grep -qE "forge (WARN|FAIL)" "$WORK/serve.log" && fail "serve's startup checks complained: $(grep -E 'forge (WARN|FAIL)' "$WORK/serve.log")"

echo "== 6: contexts = [] ignores the PR's own runs"
kill "$SERVE_PID"; wait "$SERVE_PID" 2>/dev/null; SERVE_PID=
sed -i.bak 's/^contexts = .*/contexts = []/; /^poll_interval = 3600/d; /^listen = /d' "$WORK/m.toml"
read -r own own_sha < <(pr own c.txt)
statuses() { api "$R" "$API/repos/root/app/commits/$1/statuses" | json '" ".join(sorted(s["context"]+"="+s["status"] for s in d))'; }
for _ in $(seq 1 90); do [[ $(statuses "$own_sha") == *"ci / lint (pull_request)=failure"* ]] && break; sleep 1; done
[[ $(statuses "$own_sha") == *"ci / lint (pull_request)=failure"* ]] || fail "the PR's own lint run did not fail"
say "$own" "@murasame approve $own_sha"
tick
[[ $(merged "$own") == True ]] || fail "#$own did not land: its pull_request run's failure decided (see $WORK/tick.log)"

echo "== 7: one serve, two repositories"
api "$R" -d '{"name":"docs","auto_init":true,"default_branch":"main"}' "$API/user/repos" >/dev/null
for u in murasame alice; do api "$R" -X PUT -d '{"permission":"write"}' "$API/repos/root/docs/collaborators/$u"; done
api "$R" -X PATCH -d '{"allow_manual_merge":true}' "$API/repos/root/docs" >/dev/null
sed 's#root/app#root/docs#; s/^repo = "app"/repo = "docs"/' "$WORK/m.toml" >"$WORK/docs.toml"
if MURASAME_FORGEJO_TOKEN=$B "$WORK/murasame" check-config --forge --config "$WORK/docs.toml" >"$WORK/check-docs.log" 2>&1; then
  fail "check-config --forge passed a repository without a workflow"
fi
grep -q "no workflow in .forgejo/workflows" "$WORK/check-docs.log" || fail "check-config --forge missed the workflow"
api "$R" -d @"$WORK/body.json" "$API/repos/root/docs/contents/.forgejo/workflows/ci.yml" >/dev/null
git clone -q "http://root:$R@127.0.0.1:$PORT/root/docs.git" "$WORK/docs-src"
git -C "$WORK/docs-src" config user.name root
git -C "$WORK/docs-src" config user.email root@example.com
prin() { # prin REPO DIR BRANCH FILE: like pr, in another repository
  git -C "$2" fetch -q origin
  git -C "$2" checkout -q -B "$3" origin/main
  echo "$3" >"$2/$4"
  git -C "$2" add -A && git -C "$2" commit -q -m "$3"
  git -C "$2" push -q -f origin "$3" 2>/dev/null
  api "$R" -d "{\"head\":\"$3\",\"base\":\"main\",\"title\":\"$3\"}" "$API/repos/root/$1/pulls" |
    json 'd["number"], d["head"]["sha"]' | tr -d "(),'"
}
sayin() { api "$A" -d "{\"body\":\"$3\"}" "$API/repos/root/$1/issues/$2/comments" >/dev/null; }
mergedin() { api "$R" "$API/repos/root/$1/pulls/$2" | json 'd["merged"]'; }
cat >"$WORK/multi.toml" <<TOML
[instance]
state_dir = "$WORK/instance"
listen = "127.0.0.1:$SERVE_PORT"
poll_interval = 5

[forges.local]
url = "http://127.0.0.1:$PORT"
token_env = "MURASAME_FORGEJO_TOKEN"

[[repos]]
repository = "root/app"
[repos.ci]
contexts = ["ci / test (push)"]
poll_interval = 2
[repos.forgejo]
reviewers = ["alice"]

[[repos]]
repository = "root/docs"
[repos.ci]
contexts = ["ci / test (push)"]
poll_interval = 2
[repos.forgejo]
reviewers = ["alice"]
TOML
MURASAME_FORGEJO_TOKEN=$B "$WORK/murasame" check-config --config "$WORK/multi.toml" >"$WORK/multi-check.log" 2>&1 ||
  fail "the multi-repo config does not load: $(tail -1 "$WORK/multi-check.log")"
MURASAME_FORGEJO_TOKEN=$B "$WORK/murasame" serve --config "$WORK/multi.toml" >"$WORK/multi.log" 2>&1 &
SERVE_PID=$!
sleep 3
kill -0 "$SERVE_PID" 2>/dev/null || fail "serve exited: $(tail -1 "$WORK/multi.log")"
read -r app_n app_sha < <(prin app "$WORK/src" multi-app d.txt)
read -r docs_n docs_sha < <(prin docs "$WORK/docs-src" multi-docs e.txt)
sayin app "$app_n" "@murasame approve $app_sha"
sayin docs "$docs_n" "@murasame approve $docs_sha"
for _ in $(seq 1 120); do
  [[ $(mergedin app "$app_n") == True && $(mergedin docs "$docs_n") == True ]] && break
  sleep 1
done
[[ $(mergedin app "$app_n") == True ]] || fail "root/app #$app_n did not land (see $WORK/multi.log)"
[[ $(mergedin docs "$docs_n") == True ]] || fail "root/docs #$docs_n did not land (see $WORK/multi.log)"
grep -q "^\[docs\]" "$WORK/multi.log" || fail "the multi-repo log lines are not prefixed with the repository name"

echo "== 8: a hand push to staging cancels the run: not the PR's fault"
kill "$SERVE_PID"; wait "$SERVE_PID" 2>/dev/null; SERVE_PID=
read -r slow slow_sha < <(pr slow SLOW)
say "$slow" "@murasame approve $slow_sha"
tick &
TICK_PID=$!
staged() { git ls-remote "http://root:$R@127.0.0.1:$PORT/root/app.git" refs/heads/murasame/staging | cut -f1; }
for _ in $(seq 1 60); do [[ $(staged) == "$slow_sha" ]] && break; sleep 1; done
[[ $(staged) == "$slow_sha" ]] || fail "murasame did not push #$slow to staging"
sleep 5
git -C "$WORK/src" fetch -q origin
git -C "$WORK/src" push -q -f origin origin/main:refs/heads/murasame/staging 2>/dev/null
wait "$TICK_PID" || true
grep -q "the job was cancelled" "$WORK/tick.log" || fail "the cancelled run was not reported (see $WORK/tick.log)"
grep -q "SET ASIDE #$slow" "$WORK/tick.log" && fail "#$slow was set aside for a run someone else cancelled"
[[ $(merged "$slow") == False ]] || fail "#$slow landed without a finished run"

echo "== 9: no runner"
sleep 2
docker stop "$RUN" >/dev/null
sed -i.bak 's/^timeout = .*/timeout = 25/; s/^start_timeout = .*/start_timeout = 20/' "$WORK/m.toml"
read -r idle idle_sha < <(pr idle f.txt)
say "$idle" "@murasame approve $idle_sha"
: >"$WORK/tick.log"
tick
grep -q 'never left "Waiting to run": is a runner' "$WORK/tick.log" || fail "the timeout did not point at the runner (see $WORK/tick.log)"
[[ $(merged "$idle") == False ]] || fail "#$idle landed without a runner"

echo "PASS"
