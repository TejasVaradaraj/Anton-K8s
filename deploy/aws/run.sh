#!/usr/bin/env bash
# Copies the repo to the EC2 box and runs bootstrap.sh there.
#   deploy/aws/run.sh         install / upgrade Anton
#   deploy/aws/run.sh bench   ...and run the chaos benchmark
# Options pass through: MONITORING=1 SNAPSHOTS=1 HELM_ARGS="..." deploy/aws/run.sh
# Results are copied back into ./results.
#
# The copy is a tar stream over ssh so it needs nothing but ssh and tar on
# both ends, and keeps the box's results/ folder across copies.
set -euo pipefail
cd "$(dirname "$0")"
IP=$(cat .ip)
SSH=(ssh -i anton-key.pem -o StrictHostKeyChecking=accept-new -o ServerAliveInterval=30 "ubuntu@$IP")

for _ in $(seq 1 30); do "${SSH[@]}" true 2>/dev/null && break; sleep 5; done

# Replace ~/anton with a fresh copy, keeping results/ from earlier runs.
COPYFILE_DISABLE=1 tar --no-mac-metadata --no-xattrs -C ../.. -czf - \
  --exclude .git --exclude node_modules --exclude '*.pem' --exclude deploy/aws/.instance --exclude deploy/aws/.ip \
  --exclude web/.next --exclude web/out --exclude '*.db' --exclude __pycache__ --exclude .DS_Store --exclude results . |
  "${SSH[@]}" 'set -e; rm -rf anton.new; mkdir anton.new; tar -xzf - -C anton.new;
               if [ -d anton/results ]; then mv anton/results anton.new/; fi; rm -rf anton; mv anton.new anton'

"${SSH[@]}" "cd anton && MONITORING=${MONITORING:-0} SNAPSHOTS=${SNAPSHOTS:-0} HELM_ARGS='${HELM_ARGS:-}' bash deploy/bootstrap.sh ${1:-}"

mkdir -p ../../results
"${SSH[@]}" 'cd anton && tar -czf - results 2>/dev/null' | tar -xzf - -C ../.. 2>/dev/null || true
