'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync, spawn } = require('node:child_process');
const controller = path.join(__dirname, 'service.cjs');

function run(command, env) {
  return spawnSync(process.execPath, [controller, command], { env, encoding: 'utf8', timeout: 15000 });
}
function asyncRun(command, env) {
  return new Promise(resolve => {
    const child = spawn(process.execPath, [controller, command], { env });
    let output = '';
    child.stdout.on('data', data => output += data);
    child.stderr.on('data', data => output += data);
    child.on('exit', code => resolve({ status: code, stdout: output }));
  });
}
test('background lifecycle, duplicate/concurrent starts, stale state and startup failures', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ocr-service-'));
  const binary = path.join(dir, 'fake-router');
  fs.writeFileSync(binary, `#!${process.execPath}\nprocess.on('SIGTERM', () => { console.log('graceful shutdown'); process.exit(0); }); setInterval(() => {}, 1000);\n`, { mode: 0o700 });
  const env = { ...process.env, OCR_BINARY: binary, OCR_SERVICE_DIR: path.join(dir, 'state') };
  const statePath = path.join(env.OCR_SERVICE_DIR, 'service.json');
  const read = () => JSON.parse(fs.readFileSync(statePath));
  try {
    assert.equal(run('status', env).status, 1);
    const starts = await Promise.all([asyncRun('start', env), asyncRun('start', env)]);
    for (const result of starts) assert.equal(result.status, 0, result.stdout);
    const first = read();
    assert.equal(run('start', env).status, 0);
    assert.equal(read().child, first.child);
    assert.equal(run('status', env).status, 0);
    assert.equal(run('restart', env).status, 0);
    assert.notEqual(read().child, first.child);
    assert.equal(run('stop', env).status, 0);
    assert.equal(run('stop', env).status, 0);
    assert.match(fs.readFileSync(path.join(env.OCR_SERVICE_DIR, 'service.log'), 'utf8'), /graceful shutdown/);
    // A stale state pointing at a real unrelated PID must not signal it.
    fs.writeFileSync(statePath, JSON.stringify({ pid: process.pid, started: 'wrong-starttime' }));
    assert.equal(run('stop', env).status, 0);
    assert.equal(run('start', env).status, 0);
    assert.equal(run('stop', env).status, 0);
    assert.equal(run('start', { ...env, OCR_BINARY: path.join(dir, 'missing') }).status, 1);
    fs.writeFileSync(binary, `#!${process.execPath}\nprocess.exit(2);\n`);
    assert.equal(run('start', env).status, 1);
    assert.equal(run('status', env).status, 1);
  } finally {
    run('stop', env);
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
