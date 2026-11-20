"""Deploy only the NervusOS preview's static files; run on its VPS."""
import datetime, hashlib, json, os, pathlib, shutil, tarfile, urllib.request

root = pathlib.Path('/opt/nervus-website-preview')
dist = root / 'dist'
archive = root / 'demo-static.tar.gz'
manifest_file = root / 'dist-manifest.json'
manifest = json.loads(manifest_file.read_text())
stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%d-%H%M%S')
stage = root / ('staging-demo-' + stamp)
backup = root / 'backups' / ('dist-before-demo-upgrade-' + stamp + '.tar.gz')
sha = lambda p: hashlib.sha256(p.read_bytes()).hexdigest()
server_before = {p.name: sha(p) for p in (root / 'server').glob('*.mjs')}

stage.mkdir()
with tarfile.open(archive) as t:
    t.extractall(stage, filter='data')
for rel, expected in manifest.items():
    assert sha(stage / rel) == expected, 'Staging checksum mismatch: ' + rel
for p in stage.rglob('*'):
    p.chmod(0o755 if p.is_dir() else 0o644)
backup.parent.mkdir(exist_ok=True)
with tarfile.open(backup, 'w:gz') as t:
    t.add(dist, arcname='dist')

# Publish assets before the entry point, retaining old hashed assets for open tabs.
for p in stage.iterdir():
    if p.name in ('index.html', 'index.html.gz', 'index.html.br'):
        continue
    target = dist / p.name
    if p.is_dir():
        shutil.copytree(p, target, dirs_exist_ok=True)
    else:
        shutil.copy2(p, target)
for suffix in ('.gz', '.br'):
    old = dist / ('index.html' + suffix)
    new = stage / ('index.html' + suffix)
    if new.exists():
        os.replace(new, old)
    elif old.exists():
        old.unlink()
os.replace(stage / 'index.html', dist / 'index.html')

for rel, expected in manifest.items():
    assert sha(dist / rel) == expected, 'Deployed checksum mismatch: ' + rel
assert server_before == {p.name: sha(p) for p in (root / 'server').glob('*.mjs')}
health = json.load(urllib.request.urlopen('http://127.0.0.1:8910/api/health', timeout=10))
assert health['ok'] and health['service'] == 'nervus-website'
record = {'deployed_at_utc': stamp, 'url': 'http://127.0.0.1:8910/demo',
          'backup': str(backup), 'verified_files': len(manifest),
          'index_sha256': manifest['index.html'], 'server_files_unchanged': server_before,
          'health': health, 'service_restart_required': False,
          'old_hashed_assets_retained': True}
(root / 'demo-upgrade-deployment.json').write_text(json.dumps(record, indent=2))
shutil.rmtree(stage)
archive.unlink()
manifest_file.unlink()
print(json.dumps(record, indent=2))
