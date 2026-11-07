package cli

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Exercise the actual smoke startup helper against a busy TCP port and a
// small authenticated fixture. The full smoke run exercises the real server.
func TestSmokeNodeStartRetriesCollisionAndReportsFailure(t *testing.T) {
	for _, tool := range []string{"bash", "curl", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("needs %s", tool)
		}
	}
	b, err := os.ReadFile(filepath.Join(retentionCompatRoot(t), "scripts/smoke.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	begin, end := strings.Index(s, "smoke_port() {"), strings.Index(s, "\nsmoke_start_node smoke")
	if begin < 0 || end <= begin {
		t.Fatal("smoke startup helper missing")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "fixture")
	fixture := `#!/usr/bin/env python3
import errno,json,sys
from http.server import BaseHTTPRequestHandler,HTTPServer
args=sys.argv
node=args[args.index('--node')+1]
if node=='broken':
 print('synthetic startup failure',file=sys.stderr);sys.exit(1)
port=int(args[args.index('--addr')+1].rsplit(':',1)[1])
class H(BaseHTTPRequestHandler):
 def do_GET(self):
  if self.headers.get('Authorization')!='Bearer fixture-token':self.send_error(401);return
  self.send_response(200);self.end_headers();self.wfile.write(json.dumps({'node':node},separators=(',',':')).encode())
 def log_message(self,*args):pass
try:s=HTTPServer(('127.0.0.1',port),H)
except OSError as e:
 print('bind: address already in use' if e.errno==errno.EADDRINUSE else str(e),file=sys.stderr);sys.exit(1)
s.serve_forever()
`
	if err := os.WriteFile(bin, []byte(fixture), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "token"), []byte("fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	busy, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	free, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freePort := free.Addr().(*net.TCPAddr).Port
	free.Close()
	program := `set -euo pipefail
` + s[begin:end] + `
smoke_port() { if [ "$1" = 1 ]; then echo "$BUSY_PORT"; else echo "$FREE_PORT"; fi; }
smoke_start_node quiet "$T/data" "$T/node.log"
test "$SMOKE_PORT" = "$FREE_PORT"
kill "$SMOKE_PID"; wait "$SMOKE_PID" || true
smoke_port() { echo "$FREE_PORT"; }
if smoke_start_node broken "$T/data" "$T/broken.log"; then exit 1; fi
`
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", program)
	cmd.Env = append(os.Environ(), "T="+root, "BIN="+bin, "TOKEN=fixture-token",
		"BUSY_PORT="+strconv.Itoa(busy.Addr().(*net.TCPAddr).Port), "FREE_PORT="+strconv.Itoa(freePort))
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "synthetic startup failure") {
		t.Fatalf("startup retry/diagnostics: %v: %s", err, out)
	}
}
