#!/bin/sh

# The default dstack recording is the S3 machine-failover scenario. Keep this
# short entry point so readers do not accidentally record the obsolete
# named-volume durability story.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
exec "$SCRIPT_DIR/record-s3-failover-demo.sh" "$@"
