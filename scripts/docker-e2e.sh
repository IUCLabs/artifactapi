#!/usr/bin/env bash
# Build the artifactapi container, bring up the full stack (postgres, redis,
# minio, artifactapi) plus a static mock upstream, and run the dockerised e2e
# suite against the running product over HTTP. Tears everything down on exit.
set -euo pipefail

cd "$(dirname "$0")/.."

# Publish artifactapi on 8001 to avoid colliding with a local instance on 8000.
export ARTIFACTAPI_PORT="${ARTIFACTAPI_PORT:-8001}"
COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.e2e.yml)
API_URL="${ARTIFACTAPI_URL:-http://localhost:${ARTIFACTAPI_PORT}}"

cleanup() {
  echo "==> tearing down stack"
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> building and starting stack (postgres, redis, minio, mockupstream, artifactapi)"
"${COMPOSE[@]}" up -d --build postgres redis minio mockupstream artifactapi

echo "==> waiting for artifactapi health at ${API_URL}"
for i in $(seq 1 60); do
  if curl -fsS "${API_URL}/health" >/dev/null 2>&1; then
    echo "    healthy after ${i}s"
    break
  fi
  if [ "$i" -eq 60 ]; then
    echo "!!! artifactapi did not become healthy in time; recent logs:"
    "${COMPOSE[@]}" logs --tail=50 artifactapi
    exit 1
  fi
  sleep 1
done

echo "==> running dockerised e2e suite"
ARTIFACTAPI_URL="${API_URL}" \
MOCK_UPSTREAM_INTERNAL="${MOCK_UPSTREAM_INTERNAL:-http://mockupstream}" \
  go test -tags=dockere2e -count=1 -timeout=10m -v ./e2e-docker/...
