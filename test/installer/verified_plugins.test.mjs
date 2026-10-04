import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, readFileSync, readdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';

const source = readFileSync(new URL('../../internal/resources/scripts/install-verified-plugins.mjs', import.meta.url), 'utf8');
const bytes = 'reviewed fixture archive';
const integrity = `sha512-${createHash('sha512').update(bytes).digest('base64')}`;
const pin = { package: '@third-party/plugin', version: '1.2.3-rc.1+build.2', integrity, acceptCapabilities: true };
const mock = `
import childProcess from 'node:child_process';
import { syncBuiltinESMExports } from 'node:module';
const originalSpawn = childProcess.spawnSync;
childProcess.spawnSync = (command, args, options) => {
  if (options.timeout !== 300_000 || options.stdio[0] !== 'ignore') throw Error('missing CLI deadline/noninteractive stdin');
  return originalSpawn(command, args, {...options, timeout: process.env.SCENARIO === 'cli-timeout' ? 1000 : options.timeout});
};
syncBuiltinESMExports();
const originalTimeout = AbortSignal.timeout;
AbortSignal.timeout = (ms) => {
  if (ms !== 60_000) throw Error('missing download deadline');
  return originalTimeout(process.env.SCENARIO === 'timeout' ? 10 : ms);
};
globalThis.fetch = async (url, options) => {
  if (options.redirect !== 'manual' || !options.signal) throw Error('missing redirect/deadline protection');
  const scenario = process.env.SCENARIO;
  const metadata = { name: ${JSON.stringify(pin.package)}, version: ${JSON.stringify(pin.version)}, dist: { integrity: ${JSON.stringify(integrity)}, tarball: 'https://registry.npmjs.org/fixture.tgz' } };
  if (options.headers.accept === 'application/json') {
    if (scenario === 'metadata-integrity') metadata.dist.integrity = 'wrong';
    if (scenario === 'metadata-name') metadata.name = 'wrong';
    if (scenario === 'metadata-version') metadata.version = '1.0.0';
    if (scenario === 'origin') metadata.dist.tarball = 'https://unreviewed.invalid/archive.tgz';
    if (scenario === 'credentials') metadata.dist.tarball = 'https://user:password@registry.npmjs.org/archive.tgz';
    if (scenario === 'query') metadata.dist.tarball += '?token=bad';
    if (scenario === 'http') metadata.dist.tarball = 'http://registry.npmjs.org/archive.tgz';
    if (scenario === 'network') throw Error('network unavailable');
    if (scenario === 'redirect') return new Response('', {status: 302});
    if (scenario === 'status') return new Response('', {status: 503});
    if (scenario === 'metadata-size') return new Response('x'.repeat(1024 * 1024 + 1));
    if (scenario === 'invalid-json') return new Response('{');
    return new Response(JSON.stringify(metadata));
  }
  if (scenario === 'archive-redirect') return new Response('', {status: 302});
  if (scenario === 'declared-size') return new Response('', {headers: {'content-length': String(33 * 1024 * 1024)}});
  if (scenario === 'actual-size') return new Response(new Uint8Array(32 * 1024 * 1024 + 1));
  if (scenario === 'empty-body') return {ok: true, headers: new Headers(), body: null};
  if (scenario === 'timeout') return new Response(new ReadableStream({start(controller) {
    const timer = setTimeout(() => controller.close(), 1000);
    options.signal.addEventListener('abort', () => { clearTimeout(timer); controller.error(options.signal.reason); });
  }}));
  return new Response(scenario === 'corrupt' ? 'tampered' : ${JSON.stringify(bytes)});
};
`;

const scenarios = [
  ['success', true, true], ['no-consent', false, true], ['consent-not-required', true, true],
  ...['metadata-integrity', 'metadata-name', 'metadata-version', 'origin', 'credentials', 'query', 'http',
    'network', 'redirect', 'archive-redirect', 'status', 'metadata-size', 'invalid-json', 'declared-size',
    'actual-size', 'empty-body', 'timeout', 'corrupt'].map(name => [name, false, false]),
  ['cli-failure', false, true], ['cli-timeout', false, true], ['missing-cli', false, false],
  ...['range', 'tag', 'leading-zero', 'bad-package', 'bad-integrity', 'noncanonical-integrity', 'newline', 'duplicate', 'collision', 'empty', 'consent-type'].map(name => [name, false, false]),
];
for (const [scenario, success, invoked] of scenarios) {
  test(scenario, () => {
    const root = mkdtempSync(join(tmpdir(), 'verified-plugin-test-'));
    try {
      const calls = join(root, 'calls.json');
      if (scenario !== 'missing-cli') writeFileSync(join(root, 'openclaw'), `#!${process.execPath}
const fs = require('node:fs');
const args = process.argv.slice(2);
const archive = args.at(-1).slice('npm-pack:'.length);
fs.writeFileSync(process.env.CALLS, JSON.stringify({args, bytes: fs.readFileSync(archive, 'utf8'), mode: fs.statSync(archive).mode & 0o777, scripts: process.env.NPM_CONFIG_IGNORE_SCRIPTS, lowerScripts: process.env.npm_config_ignore_scripts}));
if (process.env.SCENARIO === 'cli-timeout') setInterval(() => {}, 1000);
else if (process.env.SCENARIO === 'cli-failure') process.exit(9);
else if (!args.includes('--accept-capabilities') && process.env.SCENARIO !== 'consent-not-required') process.exit(42);
`, {mode: 0o700});
      let pins = [{...pin}];
      if (['no-consent', 'consent-not-required'].includes(scenario)) delete pins[0].acceptCapabilities;
      if (scenario === 'range') pins[0].version = '^1.2.3';
      if (scenario === 'tag') pins[0].version = 'latest';
      if (scenario === 'leading-zero') pins[0].version = '1.2.3-01';
      if (scenario === 'bad-package') pins[0].package = '../plugin';
      if (scenario === 'bad-integrity') pins[0].integrity = 'sha512-invalid';
      if (scenario === 'noncanonical-integrity') pins[0].integrity = 'sha512-' + 'A'.repeat(85) + 'B==';
      if (scenario === 'newline') pins[0].package += '\n';
      if (scenario === 'duplicate') pins.push({...pin});
      if (scenario === 'collision') pins.push({...pin, package: 'plugin'});
      if (scenario === 'empty') pins = [];
      if (scenario === 'consent-type') pins[0].acceptCapabilities = 'true';
      const result = spawnSync(process.execPath, ['--input-type=module', '--eval', mock + source, JSON.stringify(pins)], {
        env: {...process.env, PATH: root, TMPDIR: root, TMP: root, TEMP: root, SCENARIO: scenario, CALLS: calls, NPM_CONFIG_IGNORE_SCRIPTS: 'false', npm_config_ignore_scripts: 'false'},
        encoding: 'utf8', timeout: 10_000,
      });
      assert.ifError(result.error);
      assert.equal(result.status === 0, success, result.stderr);
      assert.equal(readdirSync(root).includes('calls.json'), invoked, result.stderr);
      if (invoked) {
        const call = JSON.parse(readFileSync(calls, 'utf8'));
        assert.equal(call.bytes, bytes);
        assert.equal(call.mode, 0o600);
        assert.equal(call.scripts, 'true');
        assert.equal(call.lowerScripts, 'true');
        assert.deepEqual(call.args.slice(0, -1), ['plugins', 'install', '--force', ...(pins[0].acceptCapabilities ? ['--accept-capabilities'] : [])]);
      }
      assert.deepEqual(readdirSync(root).filter(name => name.startsWith('verified-plugin-')), [], 'scratch must be cleaned up');
    } finally { rmSync(root, {recursive: true, force: true}); }
  });
}
