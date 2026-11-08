#!/usr/bin/env bash
# Renders the agent chart with runner.image_registry set and fails if any image
# stays outside that registry, or if an enabled subchart can slip through the
# chart's own validation. Run from anywhere; needs helm and built chart deps.
set -euo pipefail

chart="$(cd "$(dirname "$0")/../charts/nudgebee-agent" && pwd)"
registry="registry.example.com/mirror"
base=(--set runner.nudgebee.auth_secret_key=test
  --set nodeAgent.enabled=true
  --set opencost.enabled=true
  --set opentelemetry-collector.enabled=true)
subcharts=(--set "opentelemetry-collector.image.repository=$registry/opentelemetry-collector-contrib"
  --set "clickhouse.image.registry=$registry"
  --set clickhouse.image.repository=clickhouse
  --set "opencost.opencost.exporter.image.registry=$registry"
  --set opencost.opencost.exporter.image.repository=opencost)

# Trailing slash on purpose: operators paste registries both ways.
rendered="$(helm template t "$chart" "${base[@]}" "${subcharts[@]}" --set "runner.image_registry=$registry/")"

refs="$(printf '%s\n' "$rendered" | grep -E '^[[:space:]]*-?[[:space:]]*image:' | sed -E 's/^[[:space:]]*-?[[:space:]]*image:[[:space:]]*//; s/"//g' | sort -u)"
[ -n "$refs" ] || { echo "no images rendered" >&2; exit 1; }
leaks="$(printf '%s\n' "$refs" | awk -v prefix="$registry/" 'index($0, prefix) != 1')"
if [ -n "$leaks" ]; then
  echo "images outside $registry:" >&2
  printf '%s\n' "$leaks" >&2
  exit 1
fi

for env in PROFILER_IMAGE MOVER_IMAGE; do
  value="$(printf '%s\n' "$rendered" | grep -A1 -E "name: $env\$" | grep 'value:' | head -1)"
  case "$value" in
    *"$registry/"*) ;;
    *) echo "$env not on $registry: ${value:-unset}" >&2; exit 1 ;;
  esac
done

# Match the message: a render broken for any other reason must not pass here.
failure="$(helm template t "$chart" "${base[@]}" --set "runner.image_registry=$registry" 2>&1 >/dev/null || true)"
case "$failure" in
  *"still pull from ghcr"*) ;;
  *) echo "render should fail when enabled subcharts still pull from ghcr: ${failure:-rendered cleanly}" >&2; exit 1 ;;
esac

echo "image registry check passed: $(printf '%s\n' "$refs" | wc -l | tr -d ' ') images"
