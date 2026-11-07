package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func retentionCompatRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func TestCompatRetentionFlagsOnlyReachTheNewBinary(t *testing.T) {
	root := retentionCompatRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "scripts/compat.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	begin, end := strings.Index(s, "start() {"), strings.Index(s, "stop() {")
	if begin < 0 || end <= begin {
		t.Fatal("start function missing")
	}
	w := t.TempDir()
	fake := filepath.Join(w, "fake")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\0' \"$@\" >\"$COMPAT_CAPTURE\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	script := `umask 022
W=$1
NEW=$W/new
cp "$W/fake" "$NEW"
cp "$W/fake" "$W/old"
NAME=(new old); BIN=("$NEW" "$W/old"); PORT=(17940 17941); PID=()
up() { return 0; }
` + s[begin:end] + `
NEW_RETENTION_MODE=on
export COMPAT_CAPTURE=$W/new-on; start 0; wait "${PID[0]}"
export COMPAT_CAPTURE=$W/old-on; start 1; wait "${PID[1]}"
export COMPAT_CAPTURE=$W/upgrade-on; start 1 "$NEW"; wait "${PID[1]}"
NEW_RETENTION_MODE=off
export COMPAT_CAPTURE=$W/new-off; start 0; wait "${PID[0]}"
`
	cmd := exec.Command("bash", "-c", script, "fixture", w)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("real start function: %v %s", err, out)
	}
	for _, name := range []string{"d0", "d1"} {
		info, err := os.Stat(filepath.Join(w, name))
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			t.Fatalf("%s must be private with umask 022: mode=%v err=%v", name, info, err)
		}
	}
	for _, name := range []string{"new-on", "old-on", "upgrade-on", "new-off"} {
		out, err := os.ReadFile(filepath.Join(w, name))
		if err != nil {
			t.Fatal(err)
		}
		args := bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0})
		joined := string(bytes.Join(args, []byte(" ")))
		expectOn := name == "new-on" || name == "upgrade-on"
		if got := strings.Contains(joined, "--lease-retention on --lease-retention-hosts fixture-builder"); got != expectOn {
			t.Fatalf("%s got argv %q", name, joined)
		}
		if !expectOn && strings.Contains(joined, "--lease-retention") {
			t.Fatalf("old/default server received retention flags: %s", joined)
		}
	}
}

func TestCompatRetentionProofAndReadOnlyFailureContracts(t *testing.T) {
	root := retentionCompatRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "scripts/compat-retention.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	marker := "<<'PY_COMPAT_RETENTION'\n"
	at := strings.Index(s, marker)
	if at < 0 {
		t.Fatal("embedded fixture missing")
	}
	code := s[at+len(marker) : strings.LastIndex(s, "\nPY_COMPAT_RETENTION")]
	quoted, _ := json.Marshal(code)
	program := `import pathlib,tempfile,hashlib,os
ns={'__name__':'contract'}
exec(compile(` + string(quoted) + `, 'compat-retention', 'exec'),ns)
def refused(f):
 try:f()
 except (RuntimeError,FileExistsError):return
 raise SystemExit('unsafe proof or altered history accepted')
with tempfile.TemporaryDirectory(dir=os.path.realpath(tempfile.gettempdir())) as d:
 p=pathlib.Path(d);binary=p/'binary';binary.write_bytes(b'exact candidate')
 expiry=ns['expiry']
 if expiry('2026-11-07T03:00:00.1+01:00')!=expiry('2026-11-07T02:00:00.100000000Z'):raise SystemExit('expiry timezone/precision mismatch')
 if not expiry('2026-11-07T02:00:00.000000001Z')>expiry('2026-11-07T02:00:00Z'):raise SystemExit('expiry nanoseconds lost')
 if not expiry('2026-11-07T02:02:00Z')>expiry('2026-11-07T03:01:00.999999999+01:00'):raise SystemExit('extended mixed-zone expiry failed')
 refused(lambda:expiry('2026-11-07T02:00:00'))
 records=[{'origin':'x','seq':1,'msg':{'id':'0000000001','body':'raw'}}]
 ns['check_records'](records,records)
 refused(lambda:ns['check_records']([{'origin':'x','seq':1,'msg':{'id':'0000000001','body':'stub'}}],records))
 checks={k:'pass' for k in ns['ASSERTIONS']}
 target=p/'proof';identity=('a'*40,'b'*40)
 write=lambda a=checks,c=24,s=10,old=['0.15.0','0.6.0'],ident=identity:ns['write_record'](target,binary,old,c,s,a,ident)
 refused(lambda:write(a={k:v for k,v in checks.items() if k!='raw_log_preserved'}))
 refused(lambda:write(c=1));refused(lambda:write(s=1));refused(lambda:write(old=['0.6.0','0.6.0']))
 refused(lambda:write(ident=('bad','b'*40)))
 if target.exists():raise SystemExit('failed gate published proof')
 write();fields=dict(line.split('=',1) for line in target.read_text().splitlines())
 if fields['old']!='0.6.0 0.15.0' or fields['mode']!='on' or fields['binary_sha256']!=hashlib.sha256(binary.read_bytes()).hexdigest():raise SystemExit('proof not sealed')
 refused(write)
 ns['begin_record'](target)
 if target.exists():raise SystemExit('stale proof survived begin')
 target.write_text('ordinary important file')
 refused(lambda:ns['begin_record'](target))
 if target.read_text()!='ordinary important file':raise SystemExit('ordinary file deleted as stale proof')
 target.unlink()
 external=p/'external';external.write_text('preserve');target.symlink_to(external)
 refused(lambda:ns['begin_record'](target))
 if external.read_text()!='preserve':raise SystemExit('proof cleanup followed symlink')
 directory=p/'real-dir';directory.mkdir();ancestor=p/'alias';ancestor.symlink_to(directory,target_is_directory=True)
 refused(lambda:ns['begin_record'](ancestor/'proof'))
 refused(lambda:ns['write_record'](ancestor/'proof',binary,['0.6.0'],16,10,checks,identity))
 if list(directory.iterdir()):raise SystemExit('proof writer followed symlink ancestor')
print('indexed compatibility contracts passed')
`
	cmd := exec.Command("python3", "-I", "-c", program)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Python contract: %v %s", err, out)
	}
}

func TestCompatRetentionCannotReplaceDefaultOffRecord(t *testing.T) {
	root := retentionCompatRoot(t)
	p := filepath.Join(t.TempDir(), "off-record")
	if err := os.WriteFile(p, []byte("preserve default Off proof"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(root, "scripts/compat.sh"), "--new-retention-mode", "on", "--record", p)
	if err := cmd.Run(); err == nil {
		t.Fatal("On mode accepted the default Off record option")
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "preserve default Off proof" {
		t.Fatalf("existing default proof changed: %v", err)
	}
}

func TestCompatRetentionOwnedGroupsAndExitedPipeDeadline(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("production Linux WNOWAIT/proc proof")
	}
	root := retentionCompatRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "scripts/compat-retention.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	marker := "<<'PY_COMPAT_RETENTION'\n"
	at := strings.Index(s, marker)
	if at < 0 {
		t.Fatal("embedded fixture missing")
	}
	code := s[at+len(marker) : strings.LastIndex(s, "\nPY_COMPAT_RETENTION")]
	quoted, _ := json.Marshal(code)
	program := `import os,pathlib,sys,tempfile,subprocess,time,json
ns={'__name__':'contract'}
exec(compile(` + string(quoted) + `, 'compat-retention', 'exec'),ns)
p=subprocess.Popen([sys.executable,'-c','pass'],start_new_session=True);p.wait()
original=os.killpg;calls=[]
os.killpg=lambda *args:calls.append(args)
try:ns['stop'](p)
finally:os.killpg=original
if calls:raise SystemExit('already-reaped group was signalled')
with tempfile.TemporaryDirectory() as d:
 receipt=pathlib.Path(d)/'child.json'
 child="import subprocess,sys,pathlib,json,os; p=subprocess.Popen([sys.executable,'-c','import time;time.sleep(30)']); fields=pathlib.Path('/proc/'+str(p.pid)+'/stat').read_text().split(); pathlib.Path(sys.argv[1]).write_text(json.dumps({'pid':p.pid,'start':fields[21]}))"
 try:ns['run']([sys.executable,'-c',child,str(receipt)],timeout=.3)
 except RuntimeError:pass
 else:raise SystemExit('inherited pipe escaped command deadline')
 held=json.loads(receipt.read_text());path=pathlib.Path('/proc/'+str(held['pid'])+'/stat')
 for _ in range(100):
  if not path.exists():break
  fields=path.read_text().split()
  if fields[21]!=held['start'] or fields[2]=='Z':break
  time.sleep(.02)
 else:raise SystemExit('owned descendant survived cleanup')
print('owned process-group contracts passed')
`
	cmd := exec.Command("python3", "-I", "-c", program)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("process contract: %v %s", err, out)
	}
}
