#!/usr/bin/env sh
# Replays the whole demo script (four acts, including the kills and the cold
# restart) with nobody typing, so it can be recorded:
#
#   ./run-demo.sh            # once beforehand, so the embedding model is cached
#   asciinema rec demo.cast -c ./record-demo.sh
#
# Wipes ./demo-bucket so the recording starts from nothing. Uses its own ports
# (24300/24301) so a stale server on the usual ones cannot poison the run.
set -eu

HTTP_ADDR="127.0.0.1:24300"
GRPC_ADDR="127.0.0.1:24301"
INSPECT_ADDR="127.0.0.1:24102"
BUCKET="$PWD/demo-bucket"
SERVER="${POLIGN_SERVER:-polign-server}"

if ! command -v "$SERVER" >/dev/null 2>&1; then
  echo "polign-server not found. Install it with:"
  echo "  curl -fsSL https://get.polign.com | sh"
  echo "or point POLIGN_SERVER at a binary."
  exit 1
fi
if [ -z "${ANTHROPIC_API_KEY:-}" ] && [ -z "${OPENAI_API_KEY:-}" ]; then
  echo "warning: neither ANTHROPIC_API_KEY nor OPENAI_API_KEY is set; the agent will fail unless the SDK finds credentials another way"
fi
if curl -sf "http://$HTTP_ADDR/healthz" >/dev/null 2>&1; then
  echo "something is already listening on $HTTP_ADDR; stop it first"
  exit 1
fi

rm -rf "$BUCKET"
go build -o ./demo-agent .

SERVER_PID=""
trap 'kill "$SERVER_PID" 2>/dev/null || true; rm -f ./demo-agent' EXIT INT TERM

start_server() {
  "$SERVER" -store "fs:$BUCKET" -http "$HTTP_ADDR" -grpc "$GRPC_ADDR" >demo-server.log 2>&1 &
  SERVER_PID=$!
  i=0
  until curl -sf "http://$HTTP_ADDR/healthz" >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -gt 60 ] && { echo "server did not come up; see demo-server.log"; exit 1; }
    sleep 0.25
  done
}

narrate() {
  printf '\n\033[2m# %s\033[0m\n' "$1"
  sleep 1.5
}

agent() {
  ./demo-agent -polign "http://$HTTP_ADDR" -inspect "$INSPECT_ADDR" -script "$1"
}

narrate "act 1: a fresh bucket, a fresh agent. every write is durable before it is acknowledged."
start_server
agent demo/act1.txt

narrate "act 2: kill the agent. start a new one against the same server. memory is not context."
agent demo/act2.txt

narrate "act 3: kill -9 the server. restart it from nothing but the bucket."
kill -9 "$SERVER_PID" 2>/dev/null || true
wait "$SERVER_PID" 2>/dev/null || true
start_server
narrate "the server is back, primed only from the bucket. act 4: a contradiction."
agent demo/act34.txt

narrate "done. the bucket is the database."
