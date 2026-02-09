#!/bin/bash
# startup.sh -- GCP instance startup script for ephemeral GitHub Actions runners.
#
# This script is executed by systemd (scaleset-runner.service) on every boot.
# It reads the JIT configuration from GCP instance metadata and launches
# the GitHub Actions runner agent.
set -euo pipefail

JITCONFIG=$(curl -sf -H "Metadata-Flavor: Google" \
  "http://metadata.google.internal/computeMetadata/v1/instance/attributes/ACTIONS_RUNNER_INPUT_JITCONFIG")

if [ -z "$JITCONFIG" ]; then
  echo "ERROR: No JIT config found in instance metadata" >&2
  exit 1
fi

export ACTIONS_RUNNER_INPUT_JITCONFIG="$JITCONFIG"

cd /home/runner
exec su runner -c ./run.sh
