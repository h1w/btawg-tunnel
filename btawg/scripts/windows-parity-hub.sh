#!/usr/bin/env bash
# Use only on a disposable Linux hub/peer test segment. No host routes are changed.
set -euo pipefail
if [[ $# != 3 ]]; then
    printf 'Usage: %s <hub-guided Linux peer import file> <peer VPN IPv4> <echo port>\n' "$0" >&2
    exit 2
fi
import=$1
peer=$2
port=$3
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
build=$(mktemp -d)
client_pid=
trap 'if [[ -n "$client_pid" ]]; then kill "$client_pid" 2>/dev/null || :; wait "$client_pid" 2>/dev/null || :; fi; rm -rf -- "$build"' EXIT
(cd "$root"; CGO_ENABLED=0 go build -trimpath -o "$build/btawg-client" ./cmd/btawg-client)
"$build/btawg-client" -config "$import" &
client_pid=$!
python3 - "$peer" "$port" <<'PY'
import http.server
import socketserver
import threading
import time
import sys

address = sys.argv[1], int(sys.argv[2])
class Echo(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        data = b'btawg Linux peer\n'
        self.send_response(200)
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)

class Datagram(socketserver.BaseRequestHandler):
    def handle(self):
        data, sock = self.request
        sock.sendto(data, self.client_address)

for attempt in range(50):
    try:
        server = http.server.ThreadingHTTPServer(address, Echo)
        break
    except OSError:
        if attempt == 49:
            raise
        time.sleep(.2)
with server, socketserver.ThreadingUDPServer(address, Datagram) as udp:
    worker = threading.Thread(target=udp.serve_forever, daemon=True)
    worker.start()
    print(f'Linux VPN TCP/UDP echo listening on {address[0]}:{address[1]}', flush=True)
    server.serve_forever()
PY
