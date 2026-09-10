import { createHash } from 'node:crypto';
import {
  chmodSync, copyFileSync, createReadStream, lstatSync, mkdirSync,
  readFileSync, readdirSync, realpathSync, writeFileSync,
} from 'node:fs';
import { dirname, isAbsolute, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

export const ROOT_PACKAGE_NAME = '@guanzhu.me/usql';
export const PLATFORMS = Object.freeze([
  { id: 'linux-x64', os: 'linux', cpu: 'x64', binary: 'usql' },
  { id: 'linux-arm64', os: 'linux', cpu: 'arm64', binary: 'usql' },
  { id: 'darwin-x64', os: 'darwin', cpu: 'x64', binary: 'usql' },
  { id: 'darwin-arm64', os: 'darwin', cpu: 'arm64', binary: 'usql' },
  { id: 'win32-x64', os: 'win32', cpu: 'x64', binary: 'usql.exe' },
].map(Object.freeze));

const defaultRepoRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const repository = { type: 'git', url: 'https://github.com/wn0x00/usql.git' };
const numeric = '(?:0|[1-9][0-9]*)';
const prerelease = '(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)';
const semver = new RegExp(
  `^${numeric}\\.${numeric}\\.${numeric}(?:-${prerelease}(?:\\.${prerelease})*)?(?:\\+[0-9A-Za-z-]+(?:\\.[0-9A-Za-z-]+)*)?$`,
);

export function validateVersion(version) {
  if (typeof version !== 'string' || version.length > 256 || !semver.test(version)) {
    throw new Error('NPM_VERSION must be a strict semantic version, for example 0.19.12-ipass.1.');
  }
  for (const component of version.split(/[+-]/, 1)[0].split('.')) {
    if (!Number.isSafeInteger(Number(component))) {
      throw new Error('NPM_VERSION numeric components must be safe integers.');
    }
  }
  return version;
}

function assertInside(root, target) {
  const rel = relative(root, target);
  if (!rel || rel === '..' || rel.startsWith(`..${process.platform === 'win32' ? '\\' : '/'}`) || isAbsolute(rel)) {
    throw new Error(`Release path must be strictly inside the repository: ${target}`);
  }
}

// Reject existing symlinks/junctions in the whole path, including ancestors.
function assertNoLinks(target) {
  const parent = dirname(target);
  if (parent !== target) assertNoLinks(parent);
  let stat;
  try {
    stat = lstatSync(target);
  } catch (error) {
    if (error.code === 'ENOENT') return;
    throw error;
  }
  if (stat.isSymbolicLink()) throw new Error(`Release paths cannot contain symbolic links: ${target}`);
}

function ensureFile(path, description) {
  assertNoLinks(path);
  try {
    if (lstatSync(path).isFile()) return;
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
  }
  throw new Error(`Missing or invalid ${description}: ${path}`);
}

function assertEmptyOutput(outputRoot) {
  assertNoLinks(outputRoot);
  try {
    if (!lstatSync(outputRoot).isDirectory() || readdirSync(outputRoot).length > 0) {
      throw new Error(`Output directory must be absent or empty; existing files are never removed: ${outputRoot}`);
    }
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
  }
}

export async function sha256File(path) {
  const hash = createHash('sha256');
  for await (const chunk of createReadStream(path)) hash.update(chunk);
  return hash.digest('hex');
}

export async function verifyArtifact(artifactRoot, platform) {
  const binaryPath = join(artifactRoot, `binary-${platform.id}`, platform.binary);
  const checksumPath = `${binaryPath}.sha256`;
  ensureFile(binaryPath, `${platform.id} binary`);
  ensureFile(checksumPath, `${platform.id} SHA256 manifest`);
  if (lstatSync(binaryPath).size === 0) throw new Error(`Empty binary: ${binaryPath}`);
  const lines = readFileSync(checksumPath, 'utf8').trim().split(/\r?\n/);
  const match = lines.length === 1 && /^([0-9a-fA-F]{64}) [ *](\S+)$/.exec(lines[0]);
  if (!match || match[2] !== platform.binary) {
    throw new Error(`Invalid SHA256 manifest for ${platform.id}; expected one entry for ${platform.binary}.`);
  }
  const expected = match[1].toLowerCase();
  if (await sha256File(binaryPath) !== expected) throw new Error(`SHA256 mismatch for ${platform.id}.`);
  return { ...platform, binaryPath, sha256: expected };
}

function writeJson(path, value) {
  writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`, { flag: 'wx' });
}

function packageReadme(name, version, platform) {
  if (platform) {
    return `# ${name}\n\n${ROOT_PACKAGE_NAME} ${version} 的 ${platform.id} 原生程序包，由主包自动选择安装。\n\n请安装主包：\n\n\`\`\`sh\nnpm install -g ${ROOT_PACKAGE_NAME}@${version}\n\`\`\`\n\n源码： https://github.com/wn0x00/usql 。MIT 许可证见 LICENSE。\n`;
  }
  return `# ${name}\n\n通过影刀 iPaaS 执行数据库查询的 usql 定制发行包，基于 [xo/usql](https://github.com/xo/usql)，源码位于 [wn0x00/usql](https://github.com/wn0x00/usql)。\n\n需要 Node.js 20 或以上版本。支持 Linux 与 macOS 的 x64、arm64，以及 Windows x64。\n\n\`\`\`sh\nnpm install -g ${name}@${version}\nusql --version\nusql --help\n\`\`\`\n\n请保留 npm 可选依赖，它们包含对应平台的原生程序。启动器不下载程序、不执行安装脚本，并透传参数、环境变量、标准输入输出及退出状态。数据库网关配置请参阅源码仓库文档。\n\nMIT 许可证见 LICENSE。\n`;
}

export async function prepareNpmRelease({
  repoRoot = defaultRepoRoot,
  artifactRoot,
  outputRoot,
  version = process.env.NPM_VERSION,
} = {}) {
  repoRoot = resolve(repoRoot);
  artifactRoot = resolve(repoRoot, artifactRoot ?? '.release/artifacts');
  outputRoot = resolve(repoRoot, outputRoot ?? '.release/npm');
  assertInside(repoRoot, artifactRoot);
  assertInside(repoRoot, outputRoot);
  assertEmptyOutput(outputRoot);

  const sourcePath = join(repoRoot, 'npm', 'package.json');
  const launcherPath = join(repoRoot, 'npm', 'usql.mjs');
  const licensePath = join(repoRoot, 'LICENSE');
  ensureFile(sourcePath, 'npm source manifest');
  ensureFile(launcherPath, 'npm launcher');
  ensureFile(licensePath, 'license');
  const source = JSON.parse(readFileSync(sourcePath, 'utf8'));
  if (source.name !== ROOT_PACKAGE_NAME) throw new Error(`Source package name must be ${ROOT_PACKAGE_NAME}.`);
  version = validateVersion(version ?? source.version);
  const launcher = readFileSync(launcherPath);
  const license = readFileSync(licensePath);

  // Validate all five artifacts before creating any publishable directory.
  const artifacts = [];
  for (const platform of PLATFORMS) artifacts.push(await verifyArtifact(artifactRoot, platform));
  assertEmptyOutput(outputRoot);
  mkdirSync(outputRoot, { recursive: true });

  const optionalDependencies = Object.fromEntries(
    PLATFORMS.map(({ id }) => [`${ROOT_PACKAGE_NAME}-${id}`, version]),
  );
  const common = {
    version,
    license: 'MIT',
    repository,
    homepage: 'https://github.com/wn0x00/usql#readme',
    bugs: { url: 'https://github.com/wn0x00/usql/issues' },
    engines: { node: '>=20' },
    publishConfig: { access: 'public' },
  };
  const rootDir = join(outputRoot, 'root');
  mkdirSync(rootDir);
  mkdirSync(join(rootDir, 'bin'));
  writeFileSync(join(rootDir, 'bin', 'usql.mjs'), launcher, { flag: 'wx', mode: 0o755 });
  writeFileSync(join(rootDir, 'LICENSE'), license, { flag: 'wx' });
  writeFileSync(join(rootDir, 'README.md'), packageReadme(ROOT_PACKAGE_NAME, version), { flag: 'wx' });
  writeJson(join(rootDir, 'package.json'), {
    name: ROOT_PACKAGE_NAME,
    ...common,
    description: '通过影刀 iPaaS 执行数据库查询的 usql 定制发行包',
    keywords: ['usql', 'sql', 'ipass', 'yingdao'],
    type: 'module',
    bin: { usql: 'bin/usql.mjs' },
    files: ['bin/usql.mjs', 'README.md', 'LICENSE'],
    optionalDependencies,
  });

  for (const platform of artifacts) {
    const name = `${ROOT_PACKAGE_NAME}-${platform.id}`;
    const packageDir = join(outputRoot, platform.id);
    const targetBinary = join(packageDir, 'bin', platform.binary);
    mkdirSync(packageDir);
    mkdirSync(join(packageDir, 'bin'));
    copyFileSync(platform.binaryPath, targetBinary);
    // Also verify the copied bytes so a changed input cannot pass preflight only.
    if (await sha256File(targetBinary) !== platform.sha256) {
      throw new Error(`SHA256 mismatch after copying ${platform.id}. Do not publish this output.`);
    }
    if (platform.os !== 'win32') chmodSync(targetBinary, 0o755);
    writeFileSync(join(packageDir, 'LICENSE'), license, { flag: 'wx' });
    writeFileSync(join(packageDir, 'README.md'), packageReadme(name, version, platform), { flag: 'wx' });
    writeJson(join(packageDir, 'package.json'), {
      name,
      ...common,
      description: `${ROOT_PACKAGE_NAME} 的 ${platform.id} 原生程序包`,
      os: [platform.os],
      cpu: [platform.cpu],
      files: [`bin/${platform.binary}`, 'README.md', 'LICENSE'],
    });
  }
  return { name: ROOT_PACKAGE_NAME, version, outputRoot, rootDir, packages: PLATFORMS.map(({ id }) => join(outputRoot, id)) };
}

if (process.argv[1] && realpathSync(resolve(process.argv[1])) === fileURLToPath(import.meta.url)) {
  try {
    if (process.argv.length !== 2) throw new Error('Usage: NPM_VERSION=<semver> node scripts/prepare-npm-release.mjs');
    const result = await prepareNpmRelease();
    process.stdout.write(`Prepared ${result.name}@${result.version}\nOutput: ${result.outputRoot}\n`);
  } catch (error) {
    process.stderr.write(`npm release: ${error.message}\n`);
    process.exitCode = 1;
  }
}
