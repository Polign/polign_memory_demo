#!/usr/bin/env sh
# Starts a polign_db server on a local filesystem bucket and runs the memory
# agent against it. The first argument is the store; anything after it goes to
# the agent:
#
#   ./run-demo.sh                                    # fs:./demo-bucket, Claude
#   ./run-demo.sh s3://my-bucket/demo                # your own S3 bucket
#   ./run-demo.sh fs:./demo-bucket -model gpt-5      # OpenAI model
set -eu

STORE="${1:-fs:$PWD/demo-bucket}"
[ "$#" -ge 1 ] && shift
HTTP_ADDR="127.0.0.1:24100"
GRPC_ADDR="127.0.0.1:24101"
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

echo "starting polign-server on $HTTP_ADDR with store $STORE"
"$SERVER" -store "$STORE" -http "$HTTP_ADDR" -grpc "$GRPC_ADDR" >demo-server.log 2>&1 &
SERVER_PID=$!
trap 'kill "$SERVER_PID" 2>/dev/null || true' EXIT INT TERM

i=0
until curl -sf "http://$HTTP_ADDR/healthz" >/dev/null 2>&1; do
  i=$((i + 1))
  [ "$i" -gt 60 ] && { echo "server did not come up; see demo-server.log"; exit 1; }
  sleep 0.25
done
echo "server is up (log: demo-server.log)"

go run . -polign "http://$HTTP_ADDR" "$@"
