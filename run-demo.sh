#!/usr/bin/env sh
# Starts a polign_db server on the configured object store and runs the memory
# agent against it. An optional first store URI overrides the default; remaining
# arguments go to the agent:
#
#   ./run-demo.sh                                    # hosted English Wikipedia bucket
#   ./run-demo.sh fs:./demo-bucket -wikipedia-collection "" # memory-only, local
#   ./run-demo.sh fs:./demo-bucket -model gpt-5      # OpenAI model
set -eu

STORE="s3://polign-demo-wiki-en/polign-v4"
case "${1:-}" in
  s3://*|gcs://*|az://*|fs:*) STORE="$1"; shift ;;
esac
HTTP_ADDR="127.0.0.1:24100"
GRPC_ADDR="127.0.0.1:24101"
SERVER="${POLIGN_SERVER:-polign-server}"
AWS_REGION="${AWS_REGION:-us-east-1}"
export AWS_REGION

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
