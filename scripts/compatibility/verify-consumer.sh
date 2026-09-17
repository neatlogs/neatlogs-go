#!/usr/bin/env bash
set -euo pipefail

mode="${1:-}"
case "$mode" in
  module|workspace|vendor|readonly) ;;
  *) echo "usage: $0 {module|workspace|vendor|readonly}" >&2; exit 2 ;;
esac

repository_root="$(git rev-parse --show-toplevel)"
temporary_root="$(mktemp -d)"
trap 'rm -rf "$temporary_root"' EXIT
consumer="$temporary_root/consumer"
mkdir -p "$consumer"

cat >"$consumer/consumer_test.go" <<'EOF'
package consumer

import (
	"context"
	"testing"

	neatlogs "github.com/neatlogs/neatlogs-go"
)

func TestInstalledPackageSurface(t *testing.T) {
	client, err := neatlogs.NewClient(context.Background(), neatlogs.Config{DisableExport: true})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil {
		t.Fatal("NewClient returned nil")
	}
}
EOF

(
  cd "$consumer"
  GOWORK=off go mod init example.com/neatlogs-compatibility-consumer
  GOWORK=off go mod edit -require=github.com/neatlogs/neatlogs-go@v0.0.0
  GOWORK=off go mod edit -replace="github.com/neatlogs/neatlogs-go=$repository_root"
)

if [ "$mode" = "workspace" ]; then
  (
    cd "$temporary_root"
    GOWORK=off go work init "$consumer"
    GOWORK="$temporary_root/go.work" go test ./consumer
  )
  exit 0
fi

(
  cd "$consumer"
  GOWORK=off go mod tidy
  case "$mode" in
    module) GOWORK=off go test ./... ;;
    readonly) GOWORK=off go test -mod=readonly ./... ;;
    vendor)
      GOWORK=off go mod vendor
      GOWORK=off go test -mod=vendor ./...
      ;;
  esac
)
