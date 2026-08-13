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

echo "==> building and starting stack (postgres, redis, minio, mockupstream(s), artifactapi)"
"${COMPOSE[@]}" up -d --build postgres redis minio mockupstream mockupstreama mockupstreamb artifactapi

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

# Resolve the compose network the artifactapi container is attached to, so the
# real-package-manager test can launch a stock distro container on the same
# network and reach artifactapi by service name.
API_CID="$("${COMPOSE[@]}" ps -q artifactapi)"
COMPOSE_NETWORK="$(docker inspect -f '{{range $k,$_ := .NetworkSettings.Networks}}{{$k}}{{end}}' "${API_CID}" 2>/dev/null || true)"

echo "==> running dockerised e2e suite (compose network: ${COMPOSE_NETWORK:-unknown})"
ARTIFACTAPI_URL="${API_URL}" \
MOCK_UPSTREAM_INTERNAL="${MOCK_UPSTREAM_INTERNAL:-http://mockupstream}" \
MOCK_UPSTREAM_A_INTERNAL="${MOCK_UPSTREAM_A_INTERNAL:-http://mockupstreama}" \
MOCK_UPSTREAM_B_INTERNAL="${MOCK_UPSTREAM_B_INTERNAL:-http://mockupstreamb}" \
ARTIFACTAPI_INTERNAL="${ARTIFACTAPI_INTERNAL:-http://artifactapi:8000}" \
COMPOSE_NETWORK="${COMPOSE_NETWORK}" \
  go test -tags=dockere2e -count=1 -timeout=10m -v ./e2e-docker/...
