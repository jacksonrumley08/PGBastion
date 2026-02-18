#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

echo "=== Building pgbastion binary ==="
cd "$PROJECT_ROOT"
PATH="$HOME/go/bin:$PATH" go build -o pgbastion ./cmd/pgbastion/

echo "=== Starting docker-compose stack ==="
cd "$SCRIPT_DIR"
docker compose up -d

echo "=== Waiting for PostgreSQL to be ready (up to 30s) ==="
for i in $(seq 1 30); do
    if docker exec pgbastion-pg1 pg_isready -U postgres > /dev/null 2>&1; then
        echo "PostgreSQL ready after ${i}s"
        break
    fi
    if [ "$i" = "30" ]; then
        echo "ERROR: PostgreSQL did not become ready in 30s"
        docker compose logs
        docker compose down
        exit 1
    fi
    sleep 1
done

echo "=== Running e2e tests ==="
cd "$SCRIPT_DIR"
PGBASTION_E2E=1 PATH="$HOME/go/bin:$PATH" go test ./... -v -count=1 -timeout 120s
result=$?

echo "=== Tearing down docker-compose stack ==="
docker compose down

# Clean up Raft data
rm -rf /tmp/pgbastion-e2e-raft

exit $result
