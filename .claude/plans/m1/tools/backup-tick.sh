#!/bin/bash
# Back up the S2 packages every 10 minutes into <scratchpad>/backups with dated names.
# Silent unless a backup fails. Usage: backup-tick.sh <scratchpad> (run it as a Monitor).
SP=${1:?usage: backup-tick.sh <scratchpad>}
REPO=$(cd "$(dirname "$0")/../../../.." && pwd)
mkdir -p "$SP/backups"
while true; do
  ts=$(date -u +%Y%m%d-%H%M%S)
  tar czf "$SP/backups/validation-$ts.tgz" -C "$REPO" internal/filter/validation 2>/dev/null || echo "backup FAILED validation-$ts"
  tar czf "$SP/backups/config-$ts.tgz" -C "$REPO" internal/config/schemaview internal/config/overlay internal/config/subst internal/config/defaults internal/config/convert internal/config/precedence internal/config/canonical 2>/dev/null || echo "backup FAILED config-$ts"
  sleep 600
done
