import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { EventEmitter } from 'node:events';
import {
  copyFileSync, existsSync, lstatSync, mkdirSync, mkdtempSync, readFileSync,
  readdirSync, realpathSync, rmSync, symlinkSync, unlinkSync, writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { basename, dirname, join, resolve } from 'node:path';
import test from 'node:test';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { applyExit, launch, resolveBinary, selectPlatform } from '../npm/usql.mjs';
import { PLATFORMS, ROOT_PACKAGE_NAME, prepareNpmRelease, validateVersion } from './prepare-npm-release.mjs';

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const launcherUrl = pathToFileURL(join(repoRoot, 'npm', 'usql.mjs')).href;
const source = JSON.parse(readFileSync(join(repoRoot, 'npm', 'package.json'), 'utf8'));
const tempRoot = realpathSync(tmpdir());

function fixture(t) {
  const root = mkdtempSync(join(tempRoot, 'usql-npm-release-'));
  t.after(() => {
    const exact = realpathSync(root);
    assert.equal(dirname(exact), tempRoot);
    assert.match(basename(exact), /^usql-npm-release-/);
    assert.equal(lstatSync(root).isSymbolicLink(), false);
    rmSync(exact, { recursive: true });
  });
  mkdirSync(join(root, 'npm'));
  copyFileSync(join(repoRoot, 'npm', 'package.json'), join(root, 'npm', 'package.json'));
  copyFileSync(join(repoRoot, 'npm', 'usql.mjs'), join(root, 'npm', 'usql.mjs'));
  copyFileSync(join(repoRoot, 'LICENSE'), join(root, 'LICENSE'));
  for (const platform of PLATFORMS) {
    const directory = join(root, '.release', 'artifacts', `binary-${platform.id}`);
    mkdirSync(directory, { recursive: true });
    const binary = Buffer.from(`fixture binary: ${platform.id}\n`);
    writeFileSync(join(directory, platform.binary), binary);
    writeFileSync(join(directory, `${platform.binary}.sha256`), `${createHash('sha256').update(binary).digest('hex')}  ${platform.binary}\n`);
  }
  return root;
}

function filesBelow(root, prefix = '') {
  return readdirSync(root, { withFileTypes: true }).flatMap((entry) => {
    const name = prefix ? `${prefix}/${entry.name}` : entry.name;
    return entry.isDirectory() ? filesBelow(join(root, entry.name), name) : [name];
  }).sort();
}

function fakeRuntime(platform = 'linux') {
  return Object.assign(new EventEmitter(), {
    platform, arch: 'x64', env: { USQL_TEST_ENV: 'preserved' }, pid: 123,
  });
}

test('accepts strict semver and rejects ambiguous or invalid versions', () => {
  for (const value of ['0.19.12-ipass.1', '1.2.3', '1.2.3-0', '1.2.3-01a.2+build.001']) {
    assert.equal(validateVersion(value), value);
  }
  for (const value of ['', ' 1.2.3', '1.2.3\n', 'v1.2.3', '01.2.3', '1.2', '1.2.3-', '1.2.3-ipass.01', '1.2.3+bad..id', '9007199254740992.0.0']) {
    assert.throws(() => validateVersion(value), /NPM_VERSION/);
  }
});

test('source configuration has usql bin and synchronized platform versions', () => {
  assert.equal(source.name, ROOT_PACKAGE_NAME);
  assert.equal(validateVersion(source.version), source.version);
  assert.equal(source.private, true);
  assert.deepEqual(source.bin, { usql: 'bin/usql.mjs' });
  assert.deepEqual(source.engines, { node: '>=20' });
  assert.deepEqual(source.optionalDependencies, Object.fromEntries(PLATFORMS.map(({ id }) => [`${ROOT_PACKAGE_NAME}-${id}`, source.version])));
  assert.equal(source.scripts, undefined);
});

test('builds six packages with one version and only allowlisted public files', async (t) => {
  const root = fixture(t);
  const marker = 'PRIVATE_TEST_SENTINEL_DO_NOT_PUBLISH';
  for (const name of ['.env', '.npmrc', 'README.md', 'private-config.json']) writeFileSync(join(root, name), marker);
  const contaminatedManifest = { ...source, _authToken: marker, scripts: { postinstall: marker }, customPrivateConfig: marker };
  writeFileSync(join(root, 'npm', 'package.json'), JSON.stringify(contaminatedManifest));
  for (const platform of PLATFORMS) {
    writeFileSync(join(root, '.release', 'artifacts', `binary-${platform.id}`, 'credentials.json'), marker);
  }
  const result = await prepareNpmRelease({ repoRoot: root, version: '0.19.12-ipass.7' });
  assert.equal(result.version, '0.19.12-ipass.7');
  assert.deepEqual(readdirSync(result.outputRoot).sort(), ['root', ...PLATFORMS.map(({ id }) => id)].sort());
  const main = JSON.parse(readFileSync(join(result.rootDir, 'package.json')));
  assert.deepEqual(main.bin, { usql: 'bin/usql.mjs' });
  assert.deepEqual(main.repository, { type: 'git', url: 'https://github.com/wn0x00/usql.git' });
  assert.equal(main.private, undefined);
  assert.deepEqual(Object.values(main.optionalDependencies), PLATFORMS.map(() => result.version));
  assert.deepEqual(filesBelow(result.rootDir), ['LICENSE', 'README.md', 'bin/usql.mjs', 'package.json']);
  assert.deepEqual(readFileSync(join(result.rootDir, 'bin', 'usql.mjs')), readFileSync(join(root, 'npm', 'usql.mjs')));
  for (const platform of PLATFORMS) {
    const packageDir = join(result.outputRoot, platform.id);
    const manifest = JSON.parse(readFileSync(join(packageDir, 'package.json')));
    assert.equal(manifest.name, `${ROOT_PACKAGE_NAME}-${platform.id}`);
    assert.equal(manifest.version, result.version);
    assert.equal(main.optionalDependencies[manifest.name], manifest.version);
    assert.deepEqual(manifest.os, [platform.os]);
    assert.deepEqual(manifest.cpu, [platform.cpu]);
    assert.equal(manifest.bin, undefined);
    assert.equal(manifest.scripts, undefined);
    assert.deepEqual(filesBelow(packageDir), ['LICENSE', 'README.md', `bin/${platform.binary}`, 'package.json']);
    assert.deepEqual(readFileSync(join(packageDir, 'bin', platform.binary)), readFileSync(join(root, '.release', 'artifacts', `binary-${platform.id}`, platform.binary)));
    if (process.platform !== 'win32' && platform.os !== 'win32') {
      assert.equal(lstatSync(join(packageDir, 'bin', platform.binary)).mode & 0o777, 0o755);
    }
  }
  for (const path of filesBelow(result.outputRoot)) {
    assert.equal(readFileSync(join(result.outputRoot, path), 'utf8').includes(marker), false, path);
  }
});

test('defaults to the source version without changing the source manifest', async (t) => {
  const root = fixture(t);
  const before = readFileSync(join(root, 'npm', 'package.json'));
  const result = await prepareNpmRelease({ repoRoot: root, version: null });
  assert.equal(result.version, source.version);
  assert.deepEqual(readFileSync(join(root, 'npm', 'package.json')), before);
});

test('uses NPM_VERSION from the environment', async (t) => {
  const root = fixture(t);
  const previous = process.env.NPM_VERSION;
  process.env.NPM_VERSION = '1.2.3-ipass.99';
  try {
    assert.equal((await prepareNpmRelease({ repoRoot: root })).version, '1.2.3-ipass.99');
  } finally {
    if (previous === undefined) delete process.env.NPM_VERSION;
    else process.env.NPM_VERSION = previous;
  }
});

test('checksum mismatches fail before producing any package', async (t) => {
  const root = fixture(t);
  writeFileSync(join(root, '.release', 'artifacts', 'binary-linux-x64', 'usql'), 'corrupt binary');
  await assert.rejects(prepareNpmRelease({ repoRoot: root }), /SHA256 mismatch for linux-x64/);
  assert.equal(existsSync(join(root, '.release', 'npm')), false);
});

test('missing checksum manifests fail before producing any package', async (t) => {
  const root = fixture(t);
  unlinkSync(join(root, '.release', 'artifacts', 'binary-darwin-arm64', 'usql.sha256'));
  await assert.rejects(prepareNpmRelease({ repoRoot: root }), /Missing or invalid darwin-arm64 SHA256 manifest/);
  assert.equal(existsSync(join(root, '.release', 'npm')), false);
});

test('checksum manifests cannot refer to another file', async (t) => {
  const root = fixture(t);
  const checksum = join(root, '.release', 'artifacts', 'binary-linux-x64', 'usql.sha256');
  writeFileSync(checksum, readFileSync(checksum, 'utf8').replace('  usql', '  ../usql'));
  await assert.rejects(prepareNpmRelease({ repoRoot: root }), /Invalid SHA256 manifest/);
});

test('invalid versions fail before producing any package', async (t) => {
  const root = fixture(t);
  await assert.rejects(prepareNpmRelease({ repoRoot: root, version: 'v0.19.12' }), /strict semantic version/);
  assert.equal(existsSync(join(root, '.release', 'npm')), false);
});

test('nonempty outputs are preserved and never cleaned recursively', async (t) => {
  const root = fixture(t);
  const output = join(root, '.release', 'npm');
  mkdirSync(output);
  writeFileSync(join(output, 'existing.txt'), 'keep me');
  await assert.rejects(prepareNpmRelease({ repoRoot: root }), /Output directory must be absent or empty/);
  assert.equal(readFileSync(join(output, 'existing.txt'), 'utf8'), 'keep me');
});

test('empty output directories are usable and outputs cannot escape the repository', async (t) => {
  const root = fixture(t);
  mkdirSync(join(root, '.release', 'npm'));
  await assert.rejects(prepareNpmRelease({ repoRoot: root, outputRoot: root }), /strictly inside the repository/);
  await assert.rejects(prepareNpmRelease({ repoRoot: root, outputRoot: '..' }), /strictly inside the repository/);
  assert.equal((await prepareNpmRelease({ repoRoot: root })).version, source.version);
});

test('output directory links are rejected', async (t) => {
  const root = fixture(t);
  const realOutput = join(root, 'real-output');
  mkdirSync(realOutput);
  symlinkSync(realOutput, join(root, '.release', 'npm'), process.platform === 'win32' ? 'junction' : 'dir');
  await assert.rejects(prepareNpmRelease({ repoRoot: root }), /cannot contain symbolic links/);
  assert.deepEqual(readdirSync(realOutput), []);
});

test('selects the correct optional dependency and binary on all five platforms', () => {
  for (const platform of PLATFORMS) {
    assert.deepEqual(selectPlatform(platform.os, platform.cpu), {
      id: platform.id,
      packageName: `${ROOT_PACKAGE_NAME}-${platform.id}`,
      binary: platform.binary,
    });
    assert.equal(resolveBinary({
      platform: platform.os,
      arch: platform.cpu,
      resolveModule(modulePath) {
        assert.equal(modulePath, `${ROOT_PACKAGE_NAME}-${platform.id}/bin/${platform.binary}`);
        return process.execPath;
      },
    }), process.execPath);
  }
});

test('unsupported platforms fail explicitly', () => {
  for (const [platform, arch] of [['freebsd', 'x64'], ['win32', 'arm64'], ['linux', 'ia32']]) {
    assert.throws(() => selectPlatform(platform, arch), /Unsupported platform/);
  }
});

test('missing optional dependencies report a useful installation error', () => {
  assert.throws(() => resolveBinary({
    platform: 'linux', arch: 'x64', resolveModule() { throw new Error('MODULE_NOT_FOUND'); },
  }), /Cannot load @guanzhu.me\/usql-linux-x64.*optional dependencies enabled/);
});

test('spawn receives exact argv, environment, inherited stdio, and no shell', async () => {
  const runtime = fakeRuntime();
  const child = new EventEmitter();
  const argv = ['-c', 'select 1; $(do-not-execute)', 'value with spaces', '中文'];
  const pending = launch(argv, {
    runtime,
    findBinary: () => '/trusted/usql',
    spawnProcess(binary, args, options) {
      assert.equal(binary, '/trusted/usql');
      assert.equal(args, argv);
      assert.equal(options.env, runtime.env);
      assert.equal(options.stdio, 'inherit');
      assert.equal(options.shell, false);
      return child;
    },
  });
  child.emit('exit', 23, null);
  assert.deepEqual(await pending, { code: 23, signal: null });
  applyExit(await pending, runtime);
  assert.equal(runtime.exitCode, 23);
  assert.equal(runtime.listenerCount('SIGTERM'), 0);
});

test('signals are forwarded and child signal termination is preserved', async () => {
  const runtime = fakeRuntime();
  const child = new EventEmitter();
  const forwarded = [];
  child.kill = (signal) => forwarded.push(signal);
  const pending = launch([], { runtime, findBinary: () => '/trusted/usql', spawnProcess: () => child });
  runtime.emit('SIGINT');
  runtime.emit('SIGTERM');
  runtime.emit('SIGHUP');
  assert.deepEqual(forwarded, ['SIGINT', 'SIGTERM', 'SIGHUP']);
  child.emit('exit', null, 'SIGTERM');
  const result = await pending;
  assert.equal(result.signal, 'SIGTERM');
  runtime.kill = (pid, signal) => {
    assert.equal(pid, runtime.pid);
    assert.equal(signal, 'SIGTERM');
    assert.equal(runtime.listenerCount(signal), 0);
  };
  applyExit(result, runtime);
  assert.equal(runtime.exitCode, 143);
});

test('unsupported signal delivery and absent exit codes cannot result in success', () => {
  const runtime = fakeRuntime('win32');
  runtime.kill = () => { throw new Error('unsupported signal'); };
  applyExit({ code: null, signal: 'SIGHUP' }, runtime);
  assert.notEqual(runtime.exitCode, 0);
  applyExit({ code: null, signal: null }, runtime);
  assert.equal(runtime.exitCode, 1);
});

test('asynchronous spawn failures reject and remove signal listeners', async () => {
  const runtime = fakeRuntime();
  const child = new EventEmitter();
  const pending = launch([], { runtime, findBinary: () => '/missing/usql', spawnProcess: () => child });
  const rejection = assert.rejects(pending, /Failed to start the usql native executable/);
  child.emit('error', new Error('ENOENT'));
  await rejection;
  assert.equal(runtime.listenerCount('SIGINT'), 0);
});

test('real child process receives argv, environment and stdin and preserves stdout, stderr and exit code', () => {
  const childProgram = 'let input = ""; for await (const chunk of process.stdin) input += chunk; process.stdout.write(JSON.stringify({args: process.argv.slice(1), env: process.env.USQL_NPM_TEST_VALUE, input})); process.stderr.write("child-stderr"); process.exitCode = 17;';
  const wrapperProgram = `import { launch, applyExit } from ${JSON.stringify(launcherUrl)}; applyExit(await launch(${JSON.stringify(['--input-type=module', '-e', childProgram, '--', 'value with spaces', '中文', ';$(literal)'])}, {findBinary: () => process.execPath}));`;
  const result = spawnSync(process.execPath, ['--input-type=module', '-e', wrapperProgram], {
    input: 'from-stdin\n',
    encoding: 'utf8',
    env: { ...process.env, USQL_NPM_TEST_VALUE: 'inherited-value' },
    shell: false,
    timeout: 15000,
  });
  assert.equal(result.error, undefined);
  assert.equal(result.status, 17, result.stderr);
  assert.equal(result.stderr, 'child-stderr');
  assert.deepEqual(JSON.parse(result.stdout), { args: ['value with spaces', '中文', ';$(literal)'], env: 'inherited-value', input: 'from-stdin\n' });
});

test('actual launcher exits nonzero when its platform dependency is missing', (t) => {
  const root = fixture(t);
  const result = spawnSync(process.execPath, [join(root, 'npm', 'usql.mjs')], { encoding: 'utf8', shell: false, timeout: 15000 });
  assert.equal(result.error, undefined);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /Cannot load|Unsupported platform/);
});

test('real child signal termination is re-raised by the launcher', { skip: process.platform === 'win32' }, () => {
  const childProgram = 'process.kill(process.pid, "SIGTERM");';
  const wrapperProgram = `import { launch, applyExit } from ${JSON.stringify(launcherUrl)}; applyExit(await launch(${JSON.stringify(['-e', childProgram])}, {findBinary: () => process.execPath}));`;
  const result = spawnSync(process.execPath, ['--input-type=module', '-e', wrapperProgram], {
    encoding: 'utf8', shell: false, timeout: 15000,
  });
  assert.equal(result.error, undefined);
  assert.equal(result.status, null);
  assert.equal(result.signal, 'SIGTERM');
});

test('npm-style symlink entry invokes the launcher', { skip: process.platform === 'win32' }, (t) => {
  const root = fixture(t);
  const link = join(root, 'usql');
  symlinkSync(join(root, 'npm', 'usql.mjs'), link);
  const result = spawnSync(process.execPath, [link], { encoding: 'utf8', shell: false, timeout: 15000 });
  assert.equal(result.error, undefined);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /Cannot load|Unsupported platform/);
});
