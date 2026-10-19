#!/usr/bin/env bash
set -euo pipefail

# Every binary must accept the args the chart passes it: a flag removed from a
# binary but still rendered by the chart crash-loops that workload on install.

chart="charts/understudy"
bin="$(mktemp -d)"
trap 'rm -rf "$bin"' EXIT

go build -o "$bin/manager" cmd/main.go
go build -o "$bin/sentinel" cmd/sentinel/main.go

rendered_args() {
  helm template smoke "$chart" --set sentinel.enabled=true --show-only "$1" |
    awk '/^ *args:/ { in_args = 1; next }
         in_args && /^ *- / { sub(/^ *- /, ""); gsub(/^"|"$/, ""); print; next }
         { in_args = 0 }'
}

status=0
for pair in templates/deployment.yaml:manager templates/sentinel.yaml:sentinel; do
  template="${pair%%:*}"
  binary="${pair#*:}"
  if ! rendered="$(rendered_args "$template")"; then
    echo "error: could not render args from $template" >&2
    status=1
    continue
  fi
  if [[ -z "$rendered" ]]; then
    echo "error: $template rendered no args for $binary" >&2
    status=1
    continue
  fi
  args=()
  while IFS= read -r arg; do args+=("$arg"); done <<< "$rendered"
  if ! output="$("$bin/$binary" "${args[@]}" -h 2>&1)"; then
    echo "error: $template passes $binary args it does not accept: ${args[*]}" >&2
    echo "$output" | head -n 1 >&2
    status=1
  fi
done

exit "$status"
