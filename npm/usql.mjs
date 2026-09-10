#!/usr/bin/env node
import { spawn } from 'node:child_process';
import { realpathSync, statSync } from 'node:fs';
import { createRequire } from 'node:module';
import { constants } from 'node:os';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const require = createRequire(import.meta.url);
const packageName = '@guanzhu.me/usql';
const platforms = new Set([
  'linux-x64', 'linux-arm64', 'darwin-x64', 'darwin-arm64', 'win32-x64',
]);

export function selectPlatform(platform = process.platform, arch = process.arch) {
  const id = `${platform}-${arch}`;
  if (!platforms.has(id)) {
    throw new Error(`Unsupported platform: ${id}. Supported platforms: ${[...platforms].join(', ')}.`);
  }
  return {
    id,
    packageName: `${packageName}-${id}`,
    binary: platform === 'win32' ? 'usql.exe' : 'usql',
  };
}

export function resolveBinary({
  platform = process.platform,
  arch = process.arch,
  resolveModule = require.resolve,
} = {}) {
  const target = selectPlatform(platform, arch);
  const modulePath = `${target.packageName}/bin/${target.binary}`;
  try {
    const binary = resolveModule(modulePath);
    if (!statSync(binary).isFile()) throw new Error('The binary is not a regular file.');
    return binary;
  } catch (cause) {
    throw new Error(
      `Cannot load ${target.packageName}. Reinstall ${packageName} with optional dependencies enabled (npm install -g ${packageName} --include=optional).`,
      { cause },
    );
  }
}

// Injectable process/spawn objects make signal and failure behavior testable on every OS.
export function launch(argv = process.argv.slice(2), {
  runtime = process,
  spawnProcess = spawn,
  findBinary = resolveBinary,
} = {}) {
  const binary = findBinary({ platform: runtime.platform, arch: runtime.arch });
  return new Promise((accept, reject) => {
    let child;
    const signals = runtime.platform === 'win32'
      ? ['SIGINT', 'SIGTERM', 'SIGBREAK']
      : ['SIGINT', 'SIGTERM', 'SIGHUP'];
    const listeners = new Map();
    const cleanup = () => {
      for (const [signal, listener] of listeners) runtime.removeListener(signal, listener);
    };
    try {
      child = spawnProcess(binary, argv, {
        stdio: 'inherit',
        env: runtime.env,
        shell: false,
      });
      for (const signal of signals) {
        const listener = () => {
          try {
            child.kill(signal);
          } catch (cause) {
            cleanup();
            reject(new Error(`Failed to forward ${signal} to usql.`, { cause }));
          }
        };
        listeners.set(signal, listener);
        runtime.on(signal, listener);
      }
      child.once('error', (cause) => {
        cleanup();
        reject(new Error('Failed to start the usql native executable.', { cause }));
      });
      child.once('exit', (code, signal) => {
        cleanup();
        accept({ code: Number.isInteger(code) ? code : 1, signal });
      });
    } catch (cause) {
      cleanup();
      reject(new Error('Failed to start the usql native executable.', { cause }));
    }
  });
}

export function applyExit({ code, signal }, runtime = process) {
  if (!signal) {
    runtime.exitCode = Number.isInteger(code) ? code : 1;
    return;
  }
  // Re-raise the child's signal after removing the forwarding listeners. Windows
  // does not implement every POSIX signal, so preserve a nonzero fallback there.
  runtime.exitCode = 128 + (constants.signals[signal] || 1);
  try {
    runtime.kill(runtime.pid, signal);
  } catch {
    // The fallback exit code above also covers unsupported signal delivery.
  }
}

if (process.argv[1] && realpathSync(resolve(process.argv[1])) === fileURLToPath(import.meta.url)) {
  try {
    if (Number(process.versions.node.split('.')[0]) < 20) {
      throw new Error('usql requires Node.js 20 or newer.');
    }
    applyExit(await launch());
  } catch (error) {
    process.stderr.write(`usql: ${error.message}\n`);
    process.exitCode = 1;
  }
}
