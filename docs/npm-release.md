# npm 分发与 OIDC 发布

沿用 `wn0x00/wecom-cli` 的分发结构：`@guanzhu.me/usql` 是 Node 启动器，
通过固定版本的 `optionalDependencies` 安装当前系统对应的二进制包。
数据库网关仍独立部署，不包含在客户端 npm 包内。安装不需要 Go、编译器
或安装脚本；需要 Node.js 20 或更新版本。

## 使用

```sh
npm install -g @guanzhu.me/usql
usql --version
usql -X -J -c 'SHOW TABLES'
```

执行 SQL 前，沙箱需在当前进程环境注入 `USQL_IPASS_BASE_URL`。
省略连接参数时自动使用 `ipass://default`；授权与数据库连接串仍由服务端管理。
不要把能力 URL、数据库连接串、Token 或真实测试配置写入源码或 npm 包。

平台包共五个：

- `@guanzhu.me/usql-win32-x64`
- `@guanzhu.me/usql-linux-x64`
- `@guanzhu.me/usql-linux-arm64`
- `@guanzhu.me/usql-darwin-x64`
- `@guanzhu.me/usql-darwin-arm64`

不要禁用 npm 的 optional dependencies。命令名仍为 `usql`，因此与其他
提供同名命令的全局包不能同时占用同一个 PATH 安装位置。

## 构建与验证

发布版本统一维护在 `npm/package.json`。原始版本基于上游 `v0.19.12`，
托管修订使用 `0.19.12-ipass.N`。Go 可执行文件版本与六个 npm 包版本必须一致。

`main` 推送自动执行五平台测试、编译、SHA256 校验、npm 打包和安装验证，
生成 `npm-tarballs` artifact，但不会自动发布。发布工作流另在 Linux 上运行
数据库网关协议测试与 SQLite 集成测试。上游全驱动及 AUR/Homebrew 发布流程
仅在 `xo/usql` 执行，避免 fork 误用上游发布配置。

打包只包含声明的启动器、二进制、许可证、说明和 manifest。
发布的是已经经过安装验证的 tarball，而不是重新从整个工作区打包。

## 首次发布

新 npm 包必须先存在，才能配置 Trusted Publisher。首次发布需要有
`@guanzhu.me` 发布权限的交互登录；不要把个人 npm Token 提交或写入工作流。
从成功构建的 `npm-tarballs` artifact 下载完整 tarball，先发布五个平台包，
再发布主包，均使用 `--access public --tag latest`。

为上述六个包分别配置 Trusted Publisher：

| 字段 | 值 |
| --- | --- |
| GitHub owner | `wn0x00` |
| Repository | `usql` |
| Workflow filename | `publish-npm.yml` |
| Environment | 留空 |
| Permission | 允许 `npm publish` |

也可使用 npm 11.15 或更新版本的官方命令逐包设置（可能要求交互式二次认证）：

```sh
npm trust github @guanzhu.me/usql --repo wn0x00/usql --file publish-npm.yml --allow-publish --yes
```

五个平台包使用同样参数替换包名。仅配置这六个包，不修改企业微信包的可信关系。

## 后续发布

更新 `npm/package.json` 版本并提交到 `main`。可以手动运行
`publish-npm.yml` 并将 `publish` 设为 `true`，也可以推送
`npm-v<完整版本>` 标签。标签必须与 manifest 版本完全一致。

发布 job 使用 GitHub-hosted runner、`id-token: write` 和 npm 11，
不读取 `NPM_TOKEN`。按平台包先于主包的顺序发布；已经存在的相同版本会跳过，
非 404 的 registry 查询错误会中止，不盲目重试发布。要发布新内容必须提升版本。

官方依据：[npm Trusted Publishing](https://docs.npmjs.com/trusted-publishers/)
与 [npm trust](https://docs.npmjs.com/cli/v11/commands/npm-trust/)。
