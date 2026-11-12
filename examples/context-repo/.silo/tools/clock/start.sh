# silo runs `bash start.sh <port>` in this directory. This one builds
# the Go server into the session's state directory, outside the
# checkout, so the build leaves the working tree clean, and then
# replaces itself with the server.
set -euo pipefail
go build -o "$SILO_STATE_DIR/clock" .
exec "$SILO_STATE_DIR/clock" "$1"
