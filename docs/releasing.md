# 发布 Super Cage

一次发布：版本号 PR → 合并 → 一条命令构建并汇总全部安装包 → 草稿 Release（逐个核对摘要）→ 明确地发布。发布路径里不跑测试套件，只做每项一分钟以内的快速检查。PR 上的 CI（`ci.yml`，约 6 分钟）跑的是快速子集，见文末。

入口是 `scripts/release/release.sh`，在 Apple Silicon Mac 上运行。

## 一次性准备

- 工具：Xcode、Go、Node、uv、已登录的 `gh`。Android 安装包还需要 Android SDK（build-tools 35）和 JDK 21。
- 配置文件 `~/.config/super-cage/release.env`，从 [`scripts/release/release.env.example`](../scripts/release/release.env.example) 复制后 `chmod 600`。文件里写 Developer ID 签名身份、公证用的 App Store Connect API 密钥（.p8 路径、key id、issuer）、Android 密钥库和密码文件的路径。环境变量优先于文件。密码不写进文件，只写保存密码的文件路径。
- 默认分支上要有 GitHub 工作流 `release-build.yml`。它按指定提交构建 Windows 安装器、Linux .deb 和服务器包。Windows 部分依赖缓存里的 QEMU 运行时和对应源码（命中时约 15 分钟，未命中约 30 分钟）；`keep-release-caches.yml` 每 3 天读一次这两份缓存，防止 GitHub 在 7 天无人读取后清掉它们。

## 步骤

1. **版本号**：`scripts/release/release.sh prepare 0.1.18`
   基于 `origin/main`，在 `.claude/worktrees/release-0.1.18-prepare` 新建分支 `release-0.1.18`。版本 PR 的 CI 会跑版本绊线（`make test-version-tripwires`）：某个兼容路径约定在这一版移除时，测试会故意失败，要在同一个 PR 里移除它，或明确把期限推后一版（例如 `internal/workbench/desktop.go` 的 `olderControllerGraceEndsIn`）。修改 `scripts/release-version.mjs` 列出的版本文件，以及 README 和 docs 里的下载链接和安装包名。然后运行 `scripts/check-version.mjs` 并提交。输出里的 `check` 行是链接以外提到旧版本的地方，需要人工判断。之后按提示推送、开 PR，再用 `gh pr merge --merge --match-head-commit` 合并。
2. **构建**：`scripts/release/release.sh build 0.1.18`
   默认取 `origin/main` 的提交，可用 `--sha` 指定，该提交的 `VERSION` 必须等于版本号。以下几路并行：
   - GitHub：`gh workflow run release-build.yml -f sha=<提交>`，等它结束后下载 `release-windows`、`release-linux`、`release-server` 三个 artifact。
   - Mac：干净 clone 后运行 `build-app.sh`（Developer ID 签名），再用 `scripts/package-macos.sh` 公证并装订 App，制作 DMG，签名后公证并装订 DMG。Mihomo 核心有本地缓存，按固定的 SHA-256 校验后才使用。
   - Android（配置了 `CAGE_ANDROID_KEYSTORE` 时）：clone 后运行 `npm ci` 和 `mobile/build.sh android-release`。versionCode 默认取当天 `YYYYMMDD01`，同一天第二次发布用 `--android-code YYYYMMDD02`，它只能增大。

   完成后汇总到 `.claude/worktrees/release-0.1.18.noindex/assets/`，按 0.1.17 的格式写三个校验文件，并运行下面的检查。clone 用完即删，加 `--keep-clones` 保留。日志和每步用时在同目录的 `logs/` 下。
3. **草稿**：`scripts/release/release.sh draft 0.1.18 --notes notes.md`
   在 GitHub 上创建目标为该提交的草稿 Release，只上传三个校验文件列出的文件和校验文件本身。上传后逐个对比 GitHub 给出的 `digest` 与本地 SHA-256，缺少、多出或不一致都会报错。`--dry-run` 只打印要执行的命令。草稿已存在时只补传缺少的文件。
4. **发布**：先在网页上看一遍草稿，再运行 `scripts/release/release.sh publish 0.1.18`。
   它把草稿设为正式版并标为 latest，然后确认 tag 指向该提交、latest 就是这一版、公开下载的 `SHA256SUMS` 与本地一致。这一步从不自动执行。
5. **本机安装**（可选）：`scripts/release/release.sh install 0.1.18`
   把 DMG 里的应用装进 `/Applications`，旧版先移到 `.claude/worktrees/app-backups.noindex`。

## build 的其他选项

| 选项 | 作用 |
| --- | --- |
| `--gh-run ID` | 复用已有的工作流运行，不再新启动 |
| `--skip-gh` | 不用 GitHub，只汇总已有的产物并列出缺少的文件 |
| `--local-server` | 在本机打服务器包，GitHub 不可用时使用（约 1–2 分钟） |
| `--no-notarize` | 不向 Apple 提交，用于测试；这样的构建 `draft`、`publish` 和 `install` 都会拒绝 |
| `--android` / `--no-android` | 强制构建或跳过 APK。README 链接了 APK，缺少时链接检查会失败 |
| `--skip-mac` | 复用同一提交已完成的 Mac 产物 |
| `--fresh` | 丢弃另一个提交留下的构建目录 |

`scripts/release/release.sh assemble 0.1.18` 只重新汇总和检查，不构建。

## 用时

下面是 2026-11-23 在 M 系列 Mac 上的实测，公证部分取自 0.1.17：

| 环节 | 用时 |
| --- | --- |
| Mac 构建 | 约 3 分钟（169–201 秒） |
| 打包 DMG，不公证 | 16 秒 |
| 两次公证加装订 | 约 6 分钟（0.1.17 实测 375 秒） |
| Android | 约 25 秒 |
| 本机打服务器包（`--local-server`） | 约 45 秒 |
| GitHub 工作流 | 约 18 分钟，Windows 最慢；和 Mac 通道同时运行 |
| 下载三个 artifact（约 1.1 GB） | 约 4.5 分钟，在工作流结束后进行 |
| 汇总和全部检查（20 个文件） | 约 10 秒 |
| 草稿上传（约 1.5 GB） | 取决于网络 |

所以一次正式构建约 25 分钟，主要花在 GitHub 工作流和下载上，Mac 通道在等待期间完成。两次测试运行的实测结果：

- `build 0.1.17 --no-notarize --skip-gh --local-server`：从 clone 到检查结束共 3 分 16 秒。
- `build 0.1.17 --no-notarize --gh-run <一次已结束的运行>`：共 6 分 34 秒，汇总出 0.1.17 的全部 20 个文件。

## 检查什么

每项都在一分钟以内：

- 预期的每个文件都在，没有多余文件；按版本号推出的文件名与 0.1.17 一致，QEMU 源码名取自该提交的 `packaging/windows/dependencies.json`。
- 三个校验文件逐一通过 `shasum -c`。
- `release-metadata.json`、`linux-desktop-build-metadata.json`、`windows-desktop-build-metadata.json` 的 `version` 与 `commitSha` 都等于本次版本和提交。
- Mihomo 源码包只上传一份，Mac、Linux、Windows 三份构建副本都必须与固定的 SHA-256 逐字节相同。
- DMG：`scripts/verify-macos-release.sh` 检查签名、公证票据、Gatekeeper、版本和 macOS 客机源码；`scripts/release/check-dmg.sh` 检查已发布版本的应用内更新对镜像的要求，包括 bundle id、可执行文件名、团队、条目数和大小上限、嵌套组件签名以及 arm64。
- 服务器包和 NoVirt 包：没有 `._` 文件，没有 macOS 扩展属性；`cage-tenant` 记录的 `vcs.revision` 等于该提交且 `vcs.modified=false`。
- .deb 的结构有效；Windows 安装器小于 512 MiB。
- APK 的包名、版本、arm64-v8a、签名证书指纹和对齐。
- 该提交的 README 和 docs 里指向这一版的下载链接和安装包名，都是实际汇总出的文件。

## 不检查什么

- 不跑 Go、UI、浏览器的测试套件，也不做安装测试（.deb 安装、Windows 安装、手机模拟器）。
- PR 上的 CI 只跑快速子集：脚本和仓库检查、界面与门户单元测试、Linux/Windows 的 Go 编译与 vet、除 `internal/workbench` 和 `internal/cage` 以外的 Go 测试、版本绊线。`internal/workbench`、`internal/cage` 和浏览器冒烟测试不会自动运行：改动涉及它们时，在本机跑 `make test-go` 或对应的冒烟脚本。手动的 `full-tests.yml`（三平台完整构建和测试）目前本身不是全绿，只适合对比失败项。
- 不验证已发布版本能否通过应用内更新拿到新版，也不把服务器包的文件列表与上一版对比。需要时手动做。
- Windows 安装器未签名（文件名带 `-unsigned`），只检查名称和大小。
- iOS 和 TestFlight 不在这个流程里。
