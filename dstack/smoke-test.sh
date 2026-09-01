#!/bin/sh

set -eu

DSTACK_USERNAME="${DSTACK_USERNAME:-polign}"
DSTACK_PASSWORD="${DSTACK_PASSWORD:-local-memory-demo-password}"
MEMORY_DEMO_PORT="${MEMORY_DEMO_PORT:-18080}"
MODEL_PROVIDER="${MODEL_PROVIDER:-anthropic}"
MODEL="${MODEL:-claude-opus-5}"
ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY:-smoke-test-key-not-called}"
OPENAI_API_KEY="${OPENAI_API_KEY:-smoke-test-key-not-called}"
export DSTACK_USERNAME DSTACK_PASSWORD MEMORY_DEMO_PORT MODEL_PROVIDER MODEL
export ANTHROPIC_API_KEY OPENAI_API_KEY

cleanup() {
    docker compose -f docker-compose.yaml -f docker-compose.local.yaml down
}
trap cleanup EXIT INT TERM

docker compose -f docker-compose.yaml -f docker-compose.local.yaml up -d --build --wait

status="$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:${MEMORY_DEMO_PORT}/")"
if [ "${status}" != "401" ]; then
    echo "anonymous UI request returned ${status}, expected 401" >&2
    exit 1
fi

health="$(curl -fsS "http://127.0.0.1:${MEMORY_DEMO_PORT}/healthz")"
if [ "${health}" != "ok" ]; then
    echo "health response was ${health}, expected ok" >&2
    exit 1
fi

page="$(curl -fsS -u "${DSTACK_USERNAME}:${DSTACK_PASSWORD}" "http://127.0.0.1:${MEMORY_DEMO_PORT}/")"
case "${page}" in
    *"Polign memory demo"*) ;;
    *)
        echo "authenticated response did not contain the memory UI" >&2
        exit 1
        ;;
esac

memories="$(curl -fsS -u "${DSTACK_USERNAME}:${DSTACK_PASSWORD}" "http://127.0.0.1:${MEMORY_DEMO_PORT}/memories/")"
case "${memories}" in
    *"memories"*) ;;
    *)
        echo "memory inspector did not render" >&2
        exit 1
        ;;
esac

echo "Polign memory dstack smoke test passed"
