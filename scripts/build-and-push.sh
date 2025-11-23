#!/usr/bin/env bash
set -euo pipefail

# Usage:
#   scripts/build-and-push.sh -i <registry/repo:tag> [-t docker|podman] [--push]
# Examples:
#   scripts/build-and-push.sh -i myrepo/k8s-webhook-operator:dev --push
#   TOOL=podman scripts/build-and-push.sh -i ghcr.io/me/k8s-webhook-operator:dev --push
#
# Defaults:
#   - tool: docker (override with -t/--tool or TOOL env var)
#   - push: disabled unless --push is provided

TOOL="${TOOL:-docker}"
IMAGE=""
PUSH=0

usage() {
  grep "^# " "$0" | sed 's/^# //'
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -i|--image)
      IMAGE="$2"
      shift 2
      ;;
    -t|--tool)
      TOOL="$2"
      shift 2
      ;;
    --push)
      PUSH=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage
      exit 1
      ;;
  esac
done

if [[ -z "$IMAGE" ]]; then
  echo "Error: image (-i|--image) is required" >&2
  usage
  exit 1
fi

if ! command -v "$TOOL" >/dev/null 2>&1; then
  echo "Error: $TOOL not found in PATH" >&2
  exit 1
fi

echo "Building image with $TOOL: $IMAGE"
"$TOOL" build -t "$IMAGE" .

if [[ $PUSH -eq 1 ]]; then
  echo "Pushing image: $IMAGE"
  "$TOOL" push "$IMAGE"
else
  echo "Build complete (push skipped). Use --push to push."
fi
