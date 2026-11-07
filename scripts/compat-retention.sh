#!/usr/bin/env bash
# Additional indexed-server gate, called only after the full On mesh passes.
# compat.sh --new-retention-mode on --retention-record compat-on-pass.txt
# Historical binaries must be in their real signed release directories.
set -euo pipefail
python3 -I - "$@" <<'PY_COMPAT_RETENTION'
import argparse
import datetime
import hashlib
import json
import os
import pathlib
import re
import selectors
import signal
import socket
import stat
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

ASSERTIONS = ('old_readonly_exports', 'raw_log_preserved', 'vv_preserved',
              'no_tombstone_or_quarantine', 'by_id_truth', 'live_lease_renew_release',
              'strict_signed_new_writes', 'rollback_old_new_on')
AGED_MANAGED, ACTIVE_MANAGED, ORDINARY = 64, 1, 16


def require(ok, explanation):
    if not ok:
        raise RuntimeError(explanation)


def digest(path):
    return hashlib.sha256(pathlib.Path(path).read_bytes()).hexdigest()


def stop(process):
    if process is None or process.returncode is not None:
        return
    # WNOWAIT reserves an exited leader's PID until owned descendants are
    # terminated. Never signal a process group whose leader we already reaped.
    try:
        os.waitid(os.P_PID,process.pid,os.WEXITED|os.WNOHANG|os.WNOWAIT)
    except ChildProcessError:
        return
    try:
        os.killpg(process.pid,signal.SIGTERM)
    except ProcessLookupError:pass
    deadline=time.monotonic()+2
    while time.monotonic()<deadline:
        if os.waitid(os.P_PID,process.pid,os.WEXITED|os.WNOHANG|os.WNOWAIT):break
        time.sleep(.02)
    try:os.killpg(process.pid,signal.SIGKILL)
    except ProcessLookupError:pass
    process.wait(timeout=2)


def run(argv, env=None, timeout=30):
    p = subprocess.Popen(argv, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                         start_new_session=True)
    selector = selectors.DefaultSelector()
    result = [bytearray(), bytearray()]
    deadline = time.monotonic()+timeout
    for index, pipe in enumerate((p.stdout, p.stderr)):
        os.set_blocking(pipe.fileno(), False)
        selector.register(pipe, selectors.EVENT_READ, index)
    try:
        while selector.get_map():
            require(time.monotonic()<deadline, 'fixture subprocess deadline exceeded')
            for key, _ in selector.select(min(.2, max(0, deadline-time.monotonic()))):
                chunk = os.read(key.fileobj.fileno(), 65536)
                if not chunk:
                    selector.unregister(key.fileobj)
                else:
                    result[key.data].extend(chunk)
                    require(sum(map(len, result))<=4*1024*1024, 'fixture subprocess output limit')
        while not os.waitid(os.P_PID,p.pid,os.WEXITED|os.WNOHANG|os.WNOWAIT):
            require(time.monotonic()<deadline,'fixture subprocess completion deadline exceeded')
            time.sleep(.02)
        # Leader is exited but unreaped, so its process-group ID is still ours.
        try:os.killpg(p.pid,signal.SIGKILL)
        except ProcessLookupError:pass
        code = p.wait(timeout=2)
        require(code==0, 'fixture command failed: '+pathlib.Path(argv[0]).name)
        return result[0].decode()
    finally:
        selector.close()
        # Includes inherited pipes held by descendants after the parent exits.
        stop(p)
        p.stdout.close()
        p.stderr.close()
        p.wait(timeout=2)


def record_parent(path,create=False):
    p=pathlib.Path(os.path.abspath(path))
    fd=os.open(p.anchor,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
    try:
        for name in p.parts[1:-1]:
            try:nextfd=os.open(name,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
            except FileNotFoundError:
                if not create:os.close(fd);return p,None
                os.mkdir(name,0o700,dir_fd=fd);os.fsync(fd)
                nextfd=os.open(name,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
            os.close(fd);fd=nextfd
        return p,fd
    except Exception:
        os.close(fd)
        raise RuntimeError('On proof path has an unsafe ancestor') from None


def begin_record(path):
    p,parent=record_parent(path)
    if parent is None:return
    try:
        try:s=os.stat(p.name,dir_fd=parent,follow_symlinks=False)
        except FileNotFoundError:return
        require(stat.S_ISREG(s.st_mode) and s.st_uid==os.geteuid() and s.st_nlink==1,
                'refusing unsafe prior On proof')
        fd=os.open(p.name,os.O_RDONLY|os.O_NOFOLLOW,dir_fd=parent)
        with os.fdopen(fd,'rb') as f:
            actual=os.fstat(f.fileno())
            require((actual.st_dev,actual.st_ino)==(s.st_dev,s.st_ino),'On proof identity changed')
            contents=f.read(65537)
            require(len(contents)<=65536 and contents.startswith(b'format=aivoicebus-compat-on-v1\n')
                    and b'\nmode=on\n' in contents and b'\nresult=pass\n' in contents,
                    'unexpected prior proof contents')
        os.unlink(p.name,dir_fd=parent);os.fsync(parent)
    finally:os.close(parent)


def canonical_records(records):
    return sorted(records, key=lambda r: (r['origin'], r['seq']))


def check_records(records, expected):
    require(canonical_records(records)==canonical_records(expected),
            'indexed history lost, changed or stubbed an authoritative record')


def source_identity():
    if subprocess.run(['git','rev-parse','--verify','--quiet','HEAD'], stdout=subprocess.DEVNULL,
                      stderr=subprocess.DEVNULL).returncode==0:
        require(not run(['git','status','--porcelain']).strip(), 'On proof requires a clean source tree')
        commit = run(['git','rev-parse','HEAD']).strip()
        tree = run(['git','rev-parse','HEAD^{tree}']).strip()
    else:
        commit, tree = os.environ.get('COMPAT_COMMIT',''), os.environ.get('COMPAT_TREE','')
    require(re.fullmatch('[0-9a-f]{40}',commit) and re.fullmatch('[0-9a-f]{40}',tree),
            'On proof needs exact source commit and tree')
    return commit, tree


def write_record(path, binary, old, checks, scenarios, assertions, identity):
    require(set(assertions)==set(ASSERTIONS) and all(v=='pass' for v in assertions.values()),
            'all indexed retention assertions must pass before proof')
    require(checks>=8*(len(old)+1) and scenarios>=10 and old, 'full On mesh did not pass')
    require(len(old)==len(set(old)) and all(re.fullmatch(r'\d+\.\d+\.\d+', v) for v in old),
            'invalid historical release set')
    commit, tree = identity
    require(re.fullmatch('[0-9a-f]{40}',commit) and re.fullmatch('[0-9a-f]{40}',tree), 'source identity')
    fields = {'format':'aivoicebus-compat-on-v1','mode':'on','result':'pass',
              'commit':commit,'tree':tree,'binary_sha256':digest(binary),
              'checks':str(checks),'scenarios':str(scenarios),
              'old':' '.join(sorted(old,key=lambda v:tuple(map(int,v.split('.'))))),
              'aged_managed':str(AGED_MANAGED),'active_managed':str(ACTIVE_MANAGED),
              'ordinary':str(ORDINARY), **assertions,
              'at':datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')}
    p,parent=record_parent(path,create=True)
    temp='.'+p.name+'.'+uuid.uuid4().hex
    fd=os.open(temp, os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW, 0o600,dir_fd=parent)
    owned=os.fstat(fd);published=False
    try:
        with os.fdopen(fd,'w') as f:
            f.write(''.join(k+'='+v+'\n' for k,v in fields.items()));f.flush();os.fsync(f.fileno())
        os.link(temp,p.name,src_dir_fd=parent,dst_dir_fd=parent,follow_symlinks=False)
        published=True;os.fsync(parent)
    except Exception:
        if published:
            current=os.stat(p.name,dir_fd=parent,follow_symlinks=False)
            if (current.st_dev,current.st_ino)==(owned.st_dev,owned.st_ino):os.unlink(p.name,dir_fd=parent)
        raise
    finally:
        os.unlink(temp,dir_fd=parent);os.close(parent)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self,*args,**kwargs):
        return None


class Fixture:
    def __init__(self, root, new):
        self.root,self.new,self.process,self.log = pathlib.Path(root),str(new),None,None
        self.data=self.root/'data';self.data.mkdir(mode=0o700)
        self.home=self.root/'home';self.home.mkdir(mode=0o700)
        self.token=uuid.uuid4().hex+uuid.uuid4().hex
        self.token_file=self.root/'token';self.token_file.write_text(self.token);self.token_file.chmod(0o600)
        self.env={k:os.environ[k] for k in ['PATH','LANG','TZ'] if k in os.environ}
        self.env.update({'HOME':str(self.home),'AIVOICEBUS_TOKEN':self.token,'NO_PROXY':'*','no_proxy':'*'})
        self.opener=urllib.request.build_opener(urllib.request.ProxyHandler({}),NoRedirect())
        self.expected=self.seed()
        self.raw=self.data/'log.jsonl'
        self.raw.write_text(''.join(json.dumps(r,separators=(',',':'))+'\n' for r in self.expected))
        self.raw.chmod(0o600)
        self.before=digest(self.raw)

    def seed(self):
        now=datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0)
        ts=lambda d:d.strftime('%Y-%m-%dT%H:%M:%SZ')
        out=[]
        for i in range(AGED_MANAGED+ACTIVE_MANAGED+ORDINARY):
            mid='%010x'%(i+1)
            if i<AGED_MANAGED+ACTIVE_MANAGED:
                expired=i<AGED_MANAGED
                exp=now+datetime.timedelta(days=-45 if expired else 1)
                who='fixture-builder/fleet-runner.%032x'%(i+1)
                message={'id':mid,'thread':mid,'from':who,'to':who,'kind':'lease',
                         'body':'deploy:prod:fixture-key-%d\nsynthetic compatibility history'%i,
                         'at':ts(exp-datetime.timedelta(hours=1) if expired else now-datetime.timedelta(hours=1)), 'exp':ts(exp)}
            else:
                message={'id':mid,'thread':mid,'from':'fixture-reader/session','to':'fixture-other/session',
                         'body':'ordinary compatibility record %d'%i,'at':ts(now-datetime.timedelta(minutes=5))}
            out.append({'origin':'fixture-seed','seq':len(out)+1,'msg':message})
        return out

    def start(self,binary=None,strict=False):
        self.close()
        binary=str(binary or self.new)
        sock=socket.socket();sock.bind(('127.0.0.1',0));port=sock.getsockname()[1];sock.close()
        self.url='http://127.0.0.1:'+str(port)
        self.env['AIVOICEBUS_URL']=self.url
        args=[binary,'serve','--addr','127.0.0.1:'+str(port),'--data',str(self.data),
              '--token-file',str(self.token_file),'--node','fixture-local']
        if binary==self.new:
            args+=['--lease-retention','on','--lease-retention-hosts','fixture-builder']
            if strict:args+=['--require-signed']
        self.log=open(self.root/('server-'+uuid.uuid4().hex+'.log'),'wb')
        self.process=subprocess.Popen(args,env=self.env,stdout=self.log,stderr=self.log,start_new_session=True)
        deadline=time.monotonic()+15
        while time.monotonic()<deadline:
            require(not os.waitid(os.P_PID,self.process.pid,os.WEXITED|os.WNOHANG|os.WNOWAIT),'fixture server exited')
            try:
                health=self.get('/v1/health')
                require(not health.get('peers'), 'fixture unexpectedly has peers')
                if binary==self.new:
                    metrics=self.request('/metrics').decode()
                    require('aivoicebus_lease_retention_mode{mode="on"} 1' in metrics,'fixture not On')
                    match=re.search(r'^aivoicebus_lease_managed_messages (\d+)$',metrics,re.M)
                    require(match and int(match[1])>=AGED_MANAGED+ACTIVE_MANAGED,'fixture is not using managed index')
                    require((self.data/'lease-history.db').is_file(),'derived index absent')
                return
            except (OSError,urllib.error.URLError):time.sleep(.05)
        raise RuntimeError('fixture server readiness deadline')

    def request(self,path,body=None):
        require(path.startswith('/'),'fixture request path')
        headers={'Authorization':'Bearer '+self.token}
        data=None
        if body is not None:
            data=json.dumps(body).encode();headers['Content-Type']='application/json'
        req=urllib.request.Request(self.url+path,data=data,headers=headers)
        with self.opener.open(req,timeout=5) as response:
            out=response.read(4*1024*1024+1)
        require(len(out)<=4*1024*1024,'fixture HTTP response cap')
        return out

    def get(self,path):return json.loads(self.request(path))

    def client(self,binary,who,args):
        env=self.env.copy();host,agent=who.split('/',1)
        env.update({'AIVOICEBUS_HOST':host,'AIVOICEBUS_AGENT':agent,'AIVOICEBUS_ME':who})
        return run([str(binary)]+args,env=env)

    def readonly(self,binary,name):
        output=self.root/('export-'+name+'.jsonl')
        self.client(binary,'fixture-reader/export',['export','--out',str(output)])
        check_records([json.loads(l) for l in output.read_text().splitlines()],self.expected)
        require(digest(self.raw)==self.before,'read-only client changed raw history')
        health=self.get('/v1/health')
        require(health['log']=={'fixture-seed':len(self.expected)},'read-only client changed vector')
        require(health.get('quarantined',0)==0 and health.get('retracted',0)==0,'quarantine or tombstone created')
        for index in [0,AGED_MANAGED,AGED_MANAGED+ACTIVE_MANAGED]:
            expected=self.expected[index]['msg'];thread=self.get('/v1/thread?id='+expected['id'])
            held=[m for m in thread if m['id']==expected['id']]
            require(len(held)==1 and all(held[0].get(k)==v for k,v in expected.items()),'ByID/thread changed original payload')

    def leases(self,key):return [l for l in (self.get('/v1/leases') or []) if l['key']==key]

    def lease_cycle(self,binary,number):
        who='fixture-builder/fleet-runner.%032x'%(10000+number)
        key='deploy:prod:compat-lease-'+str(number)
        self.client(binary,who,['lease','take','--ttl','1m','--note','compat initial '+str(number),key])
        first=self.leases(key);require(first and all(l['by']==who for l in first),'old client lease take lost')
        acquisitions=lambda:[r['msg'] for r in map(json.loads,self.raw.read_text().splitlines()) if r.get('msg',{}).get('from')==who and r['msg'].get('body','').split('\n',1)[0]==key]
        initial=acquisitions();require(initial,'first acquisition absent from raw history')
        first_ids={m['id'] for m in initial}
        self.client(binary,who,['lease','take','--ttl','1m','--note','compat renewal '+str(number),key])
        renewed=self.leases(key)
        new_claims=[m for m in acquisitions() if m['id'] not in first_ids]
        require(renewed and new_claims and max(m['exp'] for m in new_claims)>max(m['exp'] for m in initial), 'renewal did not append a fresh extended acquisition')
        require(max(l['until'] for l in renewed)>max(l['until'] for l in first),'holder expiry did not extend')
        self.client(binary,who,['lease','release',key])
        require(not self.leases(key),'old client release left a live lease')

    def close(self):
        stop(self.process);self.process=None
        if self.log:self.log.close();self.log=None


def main():
    if len(sys.argv)==3 and sys.argv[1]=='begin':
        begin_record(sys.argv[2]);return
    parser=argparse.ArgumentParser(description='Additional indexed-server compatibility gate; no production nodes')
    parser.add_argument('--new',required=True);parser.add_argument('--old',action='append',default=[])
    parser.add_argument('--checks',type=int,required=True);parser.add_argument('--scenarios',type=int,required=True)
    parser.add_argument('--record')
    args=parser.parse_args()
    if args.record:begin_record(args.record)
    require(sys.platform.startswith('linux'),'indexed compatibility fixture requires Linux')
    def interrupted(*_):raise RuntimeError('indexed compatibility fixture interrupted')
    signal.signal(signal.SIGTERM,interrupted)
    signal.signal(signal.SIGINT,interrupted)
    require(args.checks>=8*(len(args.old)+1) and args.scenarios>=10 and args.old,'full On mesh pass required')
    new=pathlib.Path(args.new).resolve();require(new.is_file() and os.access(new,os.X_OK),'new binary absent')
    old={}
    with tempfile.TemporaryDirectory(prefix='aivoicebus-compat-verify-') as home:
        verify_env={k:os.environ[k] for k in ['PATH','LANG'] if k in os.environ}
        verify_env['HOME']=home
        for entry in args.old:
            version,sep,path=entry.partition('=')
            require(sep and re.fullmatch(r'\d+\.\d+\.\d+',version) and version not in old,'historical release argument')
            binary=pathlib.Path(path).resolve();require(binary.is_file(),'historical binary absent')
            manifest_path=binary.parent/'manifest.json';manifest_bytes=manifest_path.read_bytes()
            run([str(new),'release','verify','--dir',str(binary.parent)],env=verify_env)
            require(manifest_path.read_bytes()==manifest_bytes,'historical manifest changed during verification')
            manifest=json.loads(manifest_bytes)
            require(manifest['version']==version,'wrong historical manifest')
            require(binary.name=='aivoicebus-linux-amd64','historical binary must be signed asset')
            item=manifest['files'][binary.name]
            require(0<item['size']<=64*1024*1024,'historical binary size bound')
            old[version]=(binary,item['sha256'],item['size'])
    assertions={}
    with tempfile.TemporaryDirectory(prefix='aivoicebus-compat-on-') as root:
        # Downloaded signed assets can be0644. Execute only private copies whose
        # bytes still match the verified manifest; never chmod the shared cache.
        copies={}
        for version,(binary,expected,size) in old.items():
            with binary.open('rb') as f:contents=f.read(size+1)
            require(len(contents)==size and hashlib.sha256(contents).hexdigest()==expected,'historical binary changed after verification')
            copy=pathlib.Path(root)/('client-'+version)
            with copy.open('xb') as f:f.write(contents)
            copy.chmod(0o700);copies[version]=copy
        old=copies
        fixture=Fixture(root,new)
        try:
            fixture.start()
            fixture.readonly(new,'new')
            for version,binary in old.items():
                fixture.readonly(binary,version)
            for key in ASSERTIONS[:5]:assertions[key]='pass'
            # Roll back only to releases whose nodes hold leases (msg.LeaseSince
            # is0.6.0), then forward into the same On index/raw data directory.
            capable=[(v,b) for v,b in old.items() if tuple(map(int,v.split('.')))>=(0,6,0)]
            require(capable,'no historical lease-capable client/server supplied')
            for version,binary in capable:
                fixture.start(binary);fixture.readonly(new,'rollback-'+version)
                fixture.start();fixture.readonly(binary,'forward-'+version)
            assertions['rollback_old_new_on']='pass'
            for number,(version,binary) in enumerate(capable):fixture.lease_cycle(binary,number)
            assertions['live_lease_renew_release']='pass'
            fixture.start(strict=True)
            try:
                fixture.request('/v1/send',{'from':'fixture-builder/unsigned','to':'all','body':'unsigned compatibility probe'})
                raise RuntimeError('strict indexed server accepted unsigned write')
            except urllib.error.HTTPError as error:
                require(error.code==403,'strict write did not fail closed')
            fixture.lease_cycle(new,1000)
            assertions['strict_signed_new_writes']='pass'
            require(fixture.raw.read_bytes().startswith(''.join(json.dumps(r,separators=(',',':'))+'\n' for r in fixture.expected).encode()),'writes rewrote authoritative seed')
        finally:fixture.close()
    if args.record:write_record(args.record,new,list(old),args.checks,args.scenarios,assertions,source_identity())
    print('compat-retention: all indexed historical-reader, lease and rollback assertions passed')


if __name__=='__main__':
    try:main()
    except Exception as error:
        # Synthetic fixture contents, credentials and raw subprocess errors stay local.
        print('compat-retention: '+(str(error) if type(error) is RuntimeError else type(error).__name__),file=sys.stderr)
        raise SystemExit(1)
PY_COMPAT_RETENTION
