<div align="center">

<h1>Super Cage</h1>

在工作台左栏创建和切换自己的 Super Cage 虚拟机。桌面版管理本机环境，部署到 Linux 服务器后可在普通浏览器中使用同一套工作台界面。

[![MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

<img src="docs/tenant/images/workbench-vms.png" alt="Super Cage 工作台虚拟机面板，可查看当前虚拟机、切换和按名称创建" width="860" />

</div>

## 功能

- 每个账号只能看到和打开自己的虚拟机。点击左栏 **虚拟机** 查看当前环境和切换目标。
- 新建只填名称，首次打开时才配备运行环境。切换、刷新或退出网页保留正在运行的任务。
- 每台虚拟机的设置各自独立：代理订阅、节点、链路、代理端口和出口 IP 规则，以及 DNS、时区、语言、客机身份和共享项目都按虚拟机保存，改一台不影响其他。详见[接口契约](docs/api-contract.md#每台虚拟机的独立设置)。
- 空闲的虚拟机自动降温：先冻结（内存换出到 swap，下一个请求约 0.1 秒内恢复），长时间不用再停机（磁盘保留，打开时冷启动）。策略参考 [Dormice](https://github.com/BitMiracle-AI/Dormice)，参数见[部署指南](deploy/tenant/README.md#7-空闲冷却)。
- 服务端有两个版本：**标准版**每个工作区是一台 KVM 虚拟机；**无虚拟化版**每个工作区是一个 Incus 无特权系统容器，不需要 KVM，适合未开嵌套虚拟化的云主机和实验室服务器。
- 同一服务器上的不同账号也可复用同一系统镜像。使用 Incus 和匹配容量的 ZFS 根盘时，同一镜像指纹的虚拟机共用一份只读基础盘，各自保存账号、文件和运行时写入。

## 开始使用

1. **浏览器**打开管理员提供的 HTTPS 门户地址，登录后创建或打开第一台虚拟机。新建虚拟机使用管理员配置的默认镜像，空白镜像内的工作台账户由网关自动准备。
2. **桌面版**从 [Releases](https://github.com/sweetcornna/Super-Cage/releases/latest) 下载 Apple Silicon（macOS 14+）、Windows x64 或 Linux x86-64 安装包。签名、公证和更新方式以发布说明为准。
3. 进入工作台后，点击左栏 **虚拟机**，输入名称创建，或点击已有条目切换。桌面本机和服务器分别维护自己的虚拟机清单。

[Preview 2（`v0.1.8-preview.2`）](https://github.com/sweetcornna/Super-Cage/releases/tag/v0.1.8-preview.2) 的独立桌面安装包已退役，仅作历史留档。集成版的应用更新源改为 [`sweetcornna/Super-Cage`](https://github.com/sweetcornna/Super-Cage/releases)，原有原生更新器的签名信任规则不变。

## 部署服务端

两个版本使用同一个网关和门户，区别在于工作区的运行方式。

| | 标准版 | 无虚拟化版 |
| --- | --- | --- |
| 工作区 | Incus 虚拟机 | Incus 无特权系统容器 |
| 宿主要求 | Linux，支持 KVM | Linux，不需要 KVM |
| 隔离 | 独立内核 | 与宿主共用内核，弱于虚拟机 |
| 下载 | `Super-Cage-Server-<版本>-linux-<架构>.tar.gz` | `Super-Cage-NoVirt-<版本>-linux-<架构>.tar.gz` |
| 安装 | 按部署指南手动配置 | 一条命令安装 |

v0.1.14 及更早版本的归档名以 `Cage-Tenant-` 开头，例如 `Cage-Tenant-NoVirt-0.1.14-linux-amd64.tar.gz`。

### 无虚拟化版

适用于 Debian 12 及更新、Ubuntu 22.04 及更新（包括 Ubuntu 26.04），amd64 或 arm64，需要 root 权限，不需要 KVM。建议先开启 swap，冻结的工作区靠它释放内存。

1. **下载并校验**（arm64 主机把文件名中的 `amd64` 换成 `arm64`）：

   ```sh
   base=https://github.com/sweetcornna/Super-Cage/releases/download/v0.1.16
   curl -fLO "$base/Super-Cage-NoVirt-0.1.16-linux-amd64.tar.gz" -fLO "$base/SHA256SUMS"
   sha256sum -c --ignore-missing SHA256SUMS
   tar -xzf Super-Cage-NoVirt-0.1.16-linux-amd64.tar.gz
   cd Super-Cage-NoVirt-0.1.16-linux-amd64
   ```

2. **安装**：

   ```sh
   sudo ./install.sh --origin https://cage.example.org --enable-registration
   ```

   脚本安装 Incus 6.0 LTS（Zabbly 软件源，已装则沿用），调高 inotify 等内核上限，创建存储池、带 NAT 的工作区网络和专用项目，构建空白模板 `cage-blank`，再把网关注册为 systemd 服务 `cage-tenant`（监听 `127.0.0.1:8080`）。工作区默认只能访问公网，不能访问宿主局域网。国内网络可加 `--image-mirror https://mirrors.tuna.tsinghua.edu.cn/lxc-images/ --debian-mirror http://mirrors.ustc.edu.cn`。资源上限等选项见 `./install.sh --help` 和[无虚拟化版说明](deploy/tenant/novirt/README.md)。

3. **配置域名和 TLS**：把 `cage.example.org` 和 `*.cage.example.org` 指向同一个 HTTPS 反向代理，转发到 `127.0.0.1:8080` 并开启 WebSocket，见[部署指南第 4 节](deploy/tenant/README.md#4-配置-dnstls-和反代)。
4. **关闭注册**：首批账号注册完后，不带 `--enable-registration` 再运行一次 `sudo ./install.sh --origin https://cage.example.org`。

工作区内的 Super Cage 目前停在首次配置：没有 KVM 时，它的工作环境需要预装在模板里的 Super Cage 云工作环境，安装脚本还不会构建。详见[无虚拟化版说明](deploy/tenant/novirt/README.md#验证范围)。

只想先在一台机器上试用，可以用 `sudo ./install.sh --dev`，然后在自己电脑上 `ssh -L 8080:127.0.0.1:8080 <服务器>`，浏览器打开 `http://cage.localhost:8080`。

### 标准版

服务端需要一台支持 KVM、已安装和初始化 Incus 的 Linux 主机。下面是大致步骤，具体命令和安全要求见[部署指南](deploy/tenant/README.md)。

1. **创建专用项目**：在 Incus 中创建 `cage-tenant` 项目和存储池，并设置 CPU、内存和磁盘配额。
2. **准备系统镜像**：为每个系统制作不含用户数据的空白 VM 镜像，导入项目，例如命名为 `cage-blank`。
3. **启动网关**：下载与主机架构对应的服务端归档并校验，然后运行：

   ```sh
   tar -xzf Super-Cage-Server-0.1.16-linux-amd64.tar.gz
   cd Super-Cage-Server-0.1.16-linux-amd64
   ./cage-tenant -origin https://cage.example.org \
     -state-dir /var/lib/cage-tenant -incus-image cage-blank
   ```

   网关默认监听 `127.0.0.1:8080`。状态目录的权限应为 `0700`。正式环境请配置为 systemd 服务。
4. **配置域名和 TLS**：把 `cage.example.org` 和 `*.cage.example.org` 指向同一个 TLS 反向代理，转发到 `127.0.0.1:8080`，并开启 WebSocket。部署指南里有 Nginx 示例。
5. **开放首批账号**：生产模式默认关闭注册。先在受限入口临时加上 `-enable-preview-registration`，等获准用户注册完再去掉。

## 当前状态

目标是让浏览器与桌面版的工作台界面和主要操作一致。仅原生端提供的能力需要浏览器替代方式，或明确标为平台专属；完整图形工作流的一致性仍待验证。

网关和门户仍处于预览阶段。此前在真实 Incus 环境中用 Debian 12 和 Ubuntu 24.04 验证了两个账号共三台虚拟机的隔离、切换、刷新恢复和磁盘持久化。无虚拟化版和空闲冷却在没有 `/dev/kvm` 的 arm64 测试机上验证过，范围见[无虚拟化版说明](deploy/tenant/novirt/README.md#验证范围)。完整的 Claude CLI 流程也仍待验证。

## 文档

[部署指南](deploy/tenant/README.md) · [无虚拟化版说明](deploy/tenant/novirt/README.md) · [接口契约](docs/api-contract.md)

Super Cage 原名 Claude Cage 多租户版（Cage Tenant），基于上游项目 [Lee-xeo/Claude-Cage](https://github.com/Lee-xeo/Claude-Cage)（Claude Cage，MIT）开发，同样按 MIT 许可发布，与 Anthropic 没有隶属关系。
