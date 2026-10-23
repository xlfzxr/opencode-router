#!/usr/bin/env node
'use strict';

// Dependency-free Linux/Termux child-process service controller.
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const { spawn } = require('node:child_process');
const root = path.resolve(__dirname, '..');
const dir = process.env.OCR_SERVICE_DIR || path.join(os.homedir(), '.local/state/opencode-router/service');
const stateFile = path.join(dir, 'service.json');
const logFile = path.join(dir, 'service.log');
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));

function identity(pid) {
  try {
    const stat = fs.readFileSync(`/proc/${pid}/stat`, 'utf8');
    const fields = stat.slice(stat.lastIndexOf(')') + 2).split(' ');
    if (fields[0] === 'Z') return null;
    return fields[19]; // Linux starttime (field 22), protects against PID reuse.
  } catch { return null; }
}
function readState() {
  try { return JSON.parse(fs.readFileSync(stateFile, 'utf8')); }
  catch (error) { if (error.code === 'ENOENT') return null; throw error; }
}
function alive(state) {
  return state && state.started && identity(state.pid) === state.started;
}
function save(state) {
  const tmp = `${stateFile}.${process.pid}.tmp`;
  fs.writeFileSync(tmp, JSON.stringify(state), { mode: 0o600 });
  fs.renameSync(tmp, stateFile);
}

async function supervise() {
  const state = { pid: process.pid, started: identity(process.pid), child: null };
  let child;
  let stopping = false;
  let timer;
  const cleanup = () => {
    clearTimeout(timer);
    if (readState()?.pid === process.pid) fs.unlinkSync(stateFile);
  };
  const shutdown = () => {
    if (stopping) return;
    stopping = true;
    child?.kill('SIGTERM');
    // Longer than the repository's default 10-second graceful shutdown.
    timer = setTimeout(() => child?.kill('SIGKILL'), 30000);
  };
  process.on('SIGTERM', shutdown);
  process.on('SIGINT', shutdown);
  process.on('SIGHUP', () => {});
  save(state);
  const binary = process.env.OCR_BINARY || path.join(root, 'ocr');
  const config = process.env.OCR_CONFIG || path.join(root, 'configs/config.yaml');
  child = spawn(binary, ['-config', config], { cwd: root, stdio: 'inherit' });
  child.once('error', error => {
    console.error(`Router gagal dijalankan: ${error.message}`);
    process.send?.({ error: error.message });
    cleanup();
    process.exitCode = 1;
    process.disconnect?.();
  });
  child.once('spawn', () => {
    state.child = child.pid;
    save(state);
    // Catch immediate config errors / port conflicts before reporting success.
    setTimeout(() => {
      if (!stopping && child.exitCode === null && child.signalCode === null) {
        process.send?.({ ready: true, pid: child.pid });
        if (process.connected) process.disconnect();
      }
    }, 750).unref();
  });
  child.once('exit', (code, signal) => {
    console.log(`${new Date().toISOString()} router exited (code=${code}, signal=${signal})`);
    if (process.connected) {
      process.send({ error: 'Router berhenti saat startup; periksa service logs.' });
      process.disconnect();
    }
    cleanup();
    process.exitCode = stopping ? 0 : (code || 1);
  });
}

async function start() {
  const old = readState();
  if (alive(old)) { console.log(`Service sudah berjalan (PID ${old.child || old.pid}).`); return; }
  const log = fs.openSync(logFile, 'a', 0o600);
  const daemon = spawn(process.execPath, [__filename, '__supervise'], {
    cwd: root, detached: true, stdio: ['ignore', log, log, 'ipc'], env: process.env,
  });
  fs.closeSync(log);
  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => { daemon.kill('SIGTERM'); reject(new Error('Startup timeout; periksa service logs.')); }, 10000);
    const finish = error => { clearTimeout(timer); error ? reject(error) : resolve(); };
    daemon.once('error', finish);
    daemon.once('message', message => {
      if (message.error) finish(new Error(message.error));
      else { console.log(`Service berjalan di background (PID ${message.pid}). Log: ${logFile}`); finish(); }
    });
    daemon.once('exit', () => finish(new Error('Service gagal start; periksa service logs.')));
  });
  daemon.unref();
}
async function stop() {
  const state = readState();
  if (!alive(state)) { console.log('Service tidak berjalan.'); return; }
  process.kill(state.pid, 'SIGTERM');
  for (let i = 0; i < 350; i++) {
    if (!alive(state)) { console.log('Service berhenti.'); return; }
    await sleep(100);
  }
  throw new Error('Stop timeout; periksa service logs.');
}

// Serialize controllers, including concurrent starts and restarts.
async function withLock(action) {
  const lock = path.join(dir, 'control.lock');
  let handle;
  for (let i = 0; i < 400; i++) {
    try {
      handle = fs.openSync(lock, 'wx', 0o600);
      fs.writeFileSync(handle, JSON.stringify({ pid: process.pid, started: identity(process.pid) }));
      break;
    } catch (error) {
      if (error.code !== 'EEXIST') throw error;
      try {
        const owner = JSON.parse(fs.readFileSync(lock, 'utf8'));
        if (!alive(owner)) fs.unlinkSync(lock);
      } catch (e) { if (!['ENOENT'].includes(e.code) && !(e instanceof SyntaxError)) throw e; }
      await sleep(100);
    }
  }
  if (handle === undefined) throw new Error('Service sedang diproses command lain; coba lagi.');
  try { await action(); }
  finally { fs.closeSync(handle); fs.unlinkSync(lock); }
}

async function main() {
  fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
  const command = process.argv[2];
  if (command === '__supervise') return supervise();
  if (command === 'logs') {
    const tail = spawn('tail', ['-n', '100', '-f', logFile], { stdio: 'inherit' });
    tail.on('error', error => { console.error(error.message); process.exitCode = 1; });
    tail.on('exit', code => { process.exitCode = code || 0; });
    return;
  }
  if (!['start', 'stop', 'restart', 'status'].includes(command)) {
    console.log('Penggunaan: service {start|stop|restart|status|logs}');
    process.exitCode = command ? 1 : 0;
    return;
  }
  await withLock(async () => {
    if (command === 'start') await start();
    if (command === 'stop') await stop();
    if (command === 'restart') { await stop(); await start(); }
    if (command === 'status') {
      const state = readState();
      console.log(alive(state) ? `Service berjalan (PID ${state.child || state.pid}).` : 'Service tidak berjalan.');
      if (!alive(state)) process.exitCode = 1;
    }
  });
}
main().catch(error => { console.error(error.message); process.exitCode = 1; });
