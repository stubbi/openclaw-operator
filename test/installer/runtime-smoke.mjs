// Opt-in real-image smoke test. Run only against disposable container state.
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { readFileSync, readdirSync } from 'node:fs';

const source = readFileSync(process.argv[2], 'utf8');
const pins = JSON.parse(process.argv[3]);
for (const [name, consent, success] of [
  ['missing consent', false, false],
  ['recovery after missing consent', true, true],
  ['reinstallation', true, true],
]) {
  const result = spawnSync(process.execPath, ['--input-type=module', '--eval', source,
    JSON.stringify(pins.map(pin => ({...pin, acceptCapabilities: consent})))], {
    encoding: 'utf8', timeout: 600_000,
  });
  assert.ifError(result.error);
  process.stdout.write(result.stdout);
  process.stderr.write(result.stderr);
  assert.equal(result.status === 0, success, name);
  if (!success) assert.match(result.stdout + result.stderr, /capabilit/i, 'must reproduce missing consent');
  assert.deepEqual(readdirSync('/tmp').filter(name => name.startsWith('verified-plugin-')), [], 'scratch cleanup');
  console.log(`PASS: ${name}`);
}
