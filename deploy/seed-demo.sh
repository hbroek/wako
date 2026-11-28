#!/bin/sh
# Builds a demo wiki from the pages and pictures in demo/.
#
#   deploy/seed-demo.sh [data-dir]      (default: ./demo-data)
#
# It starts a throwaway wakod on a spare local port with sign-in off, imports
# demo/ into a space called "demo", and stops it again. Afterwards run
#
#   ./bin/wakod -data-dir ./demo-data -disable-auth
#
# and open http://127.0.0.1:8888/demo/welcome. Do not expose a server run with
# -disable-auth: everyone who can reach it is an administrator.
set -eu

data=${1:-demo-data}
port=${WAKO_DEMO_PORT:-8897}
root=$(cd "$(dirname "$0")/.." && pwd)

[ -x "$root/bin/wakod" ] && [ -x "$root/bin/wako" ] || { echo "run 'make build' first" >&2; exit 1; }
if [ -e "$data" ] && [ -n "$(ls -A "$data")" ]; then
	echo "$data already has data; remove it first to start over" >&2
	exit 1
fi
mkdir -p "$data"
chmod 700 "$data"

"$root/bin/wakod" -data-dir "$data" -addr "127.0.0.1:$port" -disable-auth >"$data/seed.log" 2>&1 &
pid=$!
trap 'kill $pid 2>/dev/null || true; wait $pid 2>/dev/null || true' EXIT

i=0
until curl -fs "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 || curl -s -o /dev/null "http://127.0.0.1:$port/"; do
	i=$((i + 1))
	[ $i -lt 50 ] || { echo "the server did not start; see $data/seed.log" >&2; exit 1; }
	sleep 0.2
done

"$root/bin/wako" --server "http://127.0.0.1:$port" --space demo import --create --title "Wako demo" "$root/demo"
rm -f "$data/seed.log"
echo "done: ./bin/wakod -data-dir $data -disable-auth"
