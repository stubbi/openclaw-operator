// Install only the public npm artifact approved in the instance manifest.
import { createHash } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const registry = 'https://registry.npmjs.org';
const packagePattern = /^(@[a-z0-9][a-z0-9._-]*\/)?[a-z0-9][a-z0-9._-]*$/;
const versionPattern = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$/;
const integrityPattern = /^sha512-[A-Za-z0-9+/]{85}[AQgw]==$/;

// The deadline includes headers and the entire body; neither metadata nor
// archives may redirect or consume unbounded memory.
async function download(url, limit, accept) {
  const response = await fetch(url, {
    headers: { accept }, redirect: 'manual', signal: AbortSignal.timeout(60_000),
  });
  if (!response.ok) throw new Error(`registry returned HTTP ${response.status}`);
  const length = Number(response.headers.get('content-length'));
  if (length > limit) {
    await response.body?.cancel();
    throw new Error(`download exceeds ${limit}-byte limit`);
  }
  if (!response.body) throw new Error('registry returned no response body');
  const chunks = [];
  let size = 0;
  for await (const chunk of response.body) {
    size += chunk.byteLength;
    if (size > limit) throw new Error(`download exceeds ${limit}-byte limit`);
    chunks.push(Buffer.from(chunk));
  }
  return Buffer.concat(chunks);
}

const pins = JSON.parse(process.argv[1]);
if (!Array.isArray(pins) || pins.length === 0 || pins.length > 20) {
  throw new Error('expected 1-20 verified plugins');
}
const basenames = new Set();
// Validate the whole request before any network access or persistent mutation.
for (const pin of pins) {
  if (!pin || typeof pin.package !== 'string' || pin.package.length > 214 || !packagePattern.test(pin.package) ||
      typeof pin.version !== 'string' || pin.version.length > 128 || !versionPattern.test(pin.version) ||
      typeof pin.integrity !== 'string' || !integrityPattern.test(pin.integrity) ||
      (pin.acceptCapabilities !== undefined && typeof pin.acceptCapabilities !== 'boolean') ||
      /\s/.test(pin.package + pin.version + pin.integrity)) {
    throw new Error('invalid verified plugin package, exact version, integrity, or consent');
  }
  const basename = pin.package.split('/').at(-1);
  if (basenames.has(basename)) throw new Error(`duplicate plugin basename: ${basename}`);
  basenames.add(basename);
}

for (const pin of pins) {
  const spec = `${pin.package}@${pin.version}`;
  try {
    const metadata = JSON.parse(await download(
      `${registry}/${encodeURIComponent(pin.package)}/${encodeURIComponent(pin.version)}`,
      1024 * 1024, 'application/json',
    ));
    if (metadata.name !== pin.package || metadata.version !== pin.version) {
      throw new Error('registry returned mismatched package metadata');
    }
    if (metadata.dist?.integrity !== pin.integrity) {
      throw new Error('registry dist.integrity does not match the committed pin');
    }
    const url = new URL(metadata.dist.tarball);
    if (url.origin !== registry || url.username || url.password || url.search || url.hash) {
      throw new Error('tarball must be on registry.npmjs.org without credentials, query, or fragment');
    }
    const archive = await download(url, 32 * 1024 * 1024, 'application/octet-stream');
    const actual = `sha512-${createHash('sha512').update(archive).digest('base64')}`;
    if (actual !== pin.integrity) throw new Error('downloaded tarball SHA-512 does not match the committed pin');

    const scratch = await mkdtemp(join(tmpdir(), 'verified-plugin-'));
    try {
      const archivePath = join(scratch, 'plugin.tgz');
      await writeFile(archivePath, archive, { mode: 0o600, flag: 'wx' });
      // --force trusts the source and permits reinstallation; capability consent
      // is separate and can only be passed after verifying the pinned bytes.
      const args = ['plugins', 'install', '--force'];
      if (pin.acceptCapabilities === true) args.push('--accept-capabilities');
      args.push(`npm-pack:${archivePath}`);
      const result = spawnSync('openclaw', args, {
        stdio: ['ignore', 'inherit', 'inherit'], timeout: 300_000, killSignal: 'SIGKILL',
        env: { ...process.env, NPM_CONFIG_IGNORE_SCRIPTS: 'true', npm_config_ignore_scripts: 'true' },
      });
      if (result.error) throw result.error;
      if (result.status !== 0) throw new Error(`OpenClaw plugin install exited ${result.status} (signal ${result.signal})`);
    } finally {
      await rm(scratch, { recursive: true, force: true });
    }
    console.log(`${spec}: installed verified ${pin.integrity}`);
  } catch (error) {
    throw new Error(`${spec}: ${error.message}`, { cause: error });
  }
}
