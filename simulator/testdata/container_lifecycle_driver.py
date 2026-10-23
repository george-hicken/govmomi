#!/usr/bin/env python3
"""Delegate to the real engine; inject one declared fault for exact owned IDs."""
import fcntl
import json
import os
import pathlib
import subprocess
import sys
import time

root = pathlib.Path(os.environ['VCSIM_LIFECYCLE_CONTROL'])
cfg = json.loads((root / 'config.json').read_text())
args = sys.argv[1:]
real = cfg['engine']


def write(name, value):
    temp = root / (name + '.tmp.' + str(os.getpid()))
    temp.write_text(json.dumps(value))
    os.replace(temp, root / name)


def record(value):
    stat = pathlib.Path('/proc/self/stat').read_text().rsplit(') ', 1)[1].split()
    value.update(utc=time.time(), pid=os.getpid(), ppid=os.getppid(), pgid=os.getpgrp(), start=stat[19])
    with open(cfg['commands'], 'a') as log:
        log.write(json.dumps(value) + '\n')


def inspect(kind, target):
    p = subprocess.run([real, kind, 'inspect', target], capture_output=True, timeout=15)
    if p.returncode:
        raise RuntimeError('owned identity could not be inspected')
    return json.loads(p.stdout)[0]


def own(kind, target):
    value = inspect(kind, target)
    identity = value.get('Id', value.get('ID', value.get('id', value.get('Name', value.get('name')))))
    labels = value.get('Labels', value.get('labels', value.get('Config', {}).get('Labels', {}))) or {}
    expected = cfg['name'] if kind != 'container' else cfg['run']
    return (labels.get('container') == expected if kind != 'container' else labels.get('vcsim.run') == expected) and labels.get('runtime.control') == cfg['marker'], identity


def consume(step):
    with open(root / 'lock', 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        path = root / 'faults.json'
        faults = json.loads(path.read_text())
        if faults.get(step, 0) < 1:
            return False
        faults[step] -= 1
        write('faults.json', faults)
        return True


record(dict(event='delegate', args=args))
step = ' '.join(args[:2]) if args and args[0] in ('network', 'volume') else (args[0] if args else '')
if step in ('start', 'rm', 'network rm', 'volume rm'):
    targets = [a for a in (args[2:] if ' ' in step else args[1:]) if not a.startswith('-')]
    kind = step.split()[0] if ' ' in step else 'container'
    identities = []
    allowed = bool(targets)
    for target in targets:
        # A normal retry of rm --force for an absent container delegates unchanged.
        try:
            matched, identity = own(kind, target)
            allowed = allowed and matched
            identities.append(identity)
        except Exception:
            allowed = False
    if allowed and consume(step):
        record(dict(event='injected-failure', step=step, identities=identities))
        sys.stderr.write('declared exact-owner lifecycle control failure\n')
        sys.exit(73)

if args and args[0] == 'create' and '--name' in args and args[args.index('--name') + 1] == cfg['name']:
    begin = time.monotonic()
    result = subprocess.run([real] + args, capture_output=True, timeout=45)
    record(dict(event='create-complete', exit=result.returncode, seconds=time.monotonic() - begin))
    if result.returncode == 0:
        identity = result.stdout.decode().strip()
        matched, resolved = own('container', identity)
        if not matched or identity != resolved:
            raise RuntimeError('created identity/owner mismatch')
        value = inspect('container', identity)
        if value.get('Image', '').removeprefix('sha256:') != cfg['image'].removeprefix('sha256:'):
            raise RuntimeError('actual container image mismatch')
        volumes = []
        for mount in value.get('Mounts', []):
            if mount.get('Type') == 'volume':
                info = inspect('volume', mount['Name'])
                volumes.append(dict(Name=mount['Name'], CreatedAt=info.get('CreatedAt', '')))
        write('created.json', dict(id=identity, Volumes=volumes))
        if cfg.get('barrier'):
            write('barrier.json', dict(id=identity))
            deadline = time.monotonic() + 45
            while not (root / 'release').exists():
                if time.monotonic() > deadline:
                    raise RuntimeError('owned creation barrier timed out')
                time.sleep(.02)
    sys.stdout.buffer.write(result.stdout)
    sys.stderr.buffer.write(result.stderr)
    sys.exit(result.returncode)

# exec preserves pipe/kill ownership for the simulator's long-lived events watcher.
os.execv(real, [real] + args)
