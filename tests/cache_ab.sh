#!/bin/bash
# A/B the prompt-cache behaviour of two gateway builds on the same host.
# Usage: cache_ab.sh <binary-a> <binary-b> [repetitions]
set -u
HOST=ubuntu@15.204.94.214
A="${1:-cpa-baseline}"
B="${2:-cpa-final}"
REPS="${3:-3}"

run_side() {
  local bin="$1"
  sshpass -e ssh -o StrictHostKeyChecking=no "$HOST" "~/cachetest/run_cachetest_gw.sh $bin" >/dev/null 2>&1
  sleep 3
  for i in $(seq 1 "$REPS"); do
    CPA_BASE=http://127.0.0.1:8318 CPA_TURNS=5 python3 tests/cache_multiturn_probe.py "$bin-rep$i"
  done
}

echo "##### SIDE A: $A #####"
run_side "$A"
echo
echo "##### SIDE B: $B #####"
run_side "$B"
