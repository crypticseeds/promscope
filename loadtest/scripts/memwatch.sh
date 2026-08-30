#!/bin/sh
# Samples the promscope replicas' memory every 2s into a CSV.
#   loadtest/scripts/memwatch.sh loadtest/results/h3-stateful-mem.csv &
#   ... run the churn workload ...
#   kill %1
set -eu
OUT="${1:?usage: memwatch.sh <out.csv>}"
echo "unix_ts,container,mem_usage,mem_bytes" > "$OUT"
while true; do
  TS=$(date +%s)
  docker stats --no-stream --format '{{.Name}},{{.MemUsage}}' \
    | grep -E 'promscope-promscope-[ab]' \
    | while IFS=, read -r NAME MEM; do
        RAW=${MEM%% /*}
        # normalize MiB/GiB to bytes for plotting
        NUM=$(printf '%s' "$RAW" | sed 's/[A-Za-z]*$//')
        UNIT=$(printf '%s' "$RAW" | sed 's/^[0-9.]*//')
        case "$UNIT" in
          KiB) BYTES=$(printf '%s*1024\n' "$NUM" | bc) ;;
          MiB) BYTES=$(printf '%s*1048576\n' "$NUM" | bc) ;;
          GiB) BYTES=$(printf '%s*1073741824\n' "$NUM" | bc) ;;
          *)   BYTES=$NUM ;;
        esac
        echo "$TS,$NAME,$RAW,$BYTES" >> "$OUT"
      done
  sleep 2
done
