# Super Cage 多租户宿主部署

`cage-tenant` 在 Linux Incus 宿主机上运行。门户每个 VM 记录对应专用项目中的 `cage-<id>` 虚拟机和独立可写根盘；没有 KVM 的宿主改用 Incus 系统容器，见[第 8 节](#8-无硬件虚拟化的宿主incus-系统容器)。空闲实例由网关自动冻结和停机，见[第 7 节](#7-空闲冷却)；运行中 VM 的内存按宿主当前内存弹性调整，profile 的内存值是上限，见[第 9 节](#9-弹性内存vm-模式)。网关只通过 Incus guest agent 接入每台 VM 自己的 Super Cage 回环端口。网关为每台 VM 默认启用托管的工作台账户：门户登录后自动进入工作台，不需要再次输入密码。首次连接空白 VM 时创建该账户；已有工作台账户保持不变，继续使用原有的解锁界面和密码保险箱。

## 1. 准备 Linux 宿主机和专用项目

准备带 KVM 的 Linux 主机和已初始化的本机 Incus（没有 KVM 时按第 8 节准备）。生产可使用专门的 ZFS zpool 为 VM 根盘提供写时复制。示例使用已由管理员创建的专用 zpool `cage-zfs`，以及名为 `tenant-pool` 的 Incus 存储池。

```sh
incus --force-local storage create tenant-pool zfs source=cage-zfs volume.size=10GiB volume.zfs.reserve_space=false
incus --force-local project create cage-tenant \
  -c features.images=true -c features.profiles=true \
  -c restricted=true -c restricted.devices.disk=managed \
  -c restricted.devices.nic=block \
  -c limits.virtual-machines=20 -c limits.containers=0 \
  -c limits.cpu=80 -c limits.memory=160GiB -c limits.disk=1TiB
incus --force-local --project cage-tenant profile device add default root disk \
  path=/ pool=tenant-pool size=10GiB
incus --force-local --project cage-tenant profile set default limits.cpu=4
incus --force-local --project cage-tenant profile set default limits.memory=8GiB
incus --force-local --project cage-tenant profile show default
```

项目和每台 VM 的 CPU、内存与磁盘配额应按宿主储备设置。示例分配每台 4 vCPU、最多 8 GiB 内存和 10 GiB 根盘；VM 模式下 profile 的 `limits.memory` 是每台 VM 的上限，网关按第 9 节在上限内调整运行中 VM 的实际内存。项目的 `limits.memory` 配额按每台 VM 的上限计算。完整图形环境可能需要更大的根盘与更多内存。仅验证空闲控制器可从 2 vCPU、1 GiB 起步。

网关明确选择专用项目内的 `-incus-profile`（默认 `default`），不使用镜像关联的 profile。项目必须启用 `features.profiles=true`。网关在配备、接管及就绪复查时核对展开配置：只接受该 profile，要求显式正整数 vCPU 和绝对内存大小。vCPU 的有效值必须与 profile 一致；VM 的有效 `limits.memory` 可以是 profile 值，也可以是网关写入的实例级值，须为精确的 Incus 大小，且介于 1 GiB（profile 值更小时为 profile 值）与 profile 值之间。容器模式的内存仍须与 profile 一致。有效设备也必须一致。根盘只允许 `type=disk`、`path=/`、`pool` 和显式 `size`，不接受额外盘、宿主路径、socket、代理或其他共享设备，也不接受 `raw.*` 等自定义实例配置；唯一例外是第 9 节中开启 balloon 空闲页返还的那一条 profile 级 `raw.qemu.conf`，内容须逐字一致。已有实例若不匹配，查看网关宿主日志；保留原盘，先停机并由管理员核对配置再调整，不要临时放宽限制。不要在网关运行时改动 profile 或已运行 VM 的宿主侧配置：网关不是连续的配置审计器，也不能撤销已经生效的宿主配置。实例级 `limits.memory` 由网关自己维护（第 9 节），不要手动修改。

示例配置的租户 VM 没有网卡。每台 VM 的代理订阅、节点和出口在该 VM 自己的工作台「网络与代理」中设置，代理核心运行在 VM 内，因此 VM 需要网卡才能使用代理出口。需要联网时，仅在管理员已配置并验证受信网络后，允许在 profile 中增加一个仅含 `type=nic`、`network=<网络名>` 和可选 `name` 的受管 NIC，设置 `-incus-network` 为同一个名字，并相应调整专用项目的 NIC 限制（不要放开其他网络）。使用宿主侧规则隔离 VM 间流量，并阻止 VM 访问宿主的服务与管理网段、其他私网和云元数据地址。IPv4、IPv6 与二层转发均须在部署时验证，网关不验证这些网络规则。宿主运行 Clash、Mihomo 等带 auto-redirect 的 TUN 透明代理时，VM 的 TCP 连接会被重定向到网桥地址上的代理端口；Incus 网络 ACL 若丢弃发往私网（含网桥地址）的流量，会连带切断全部外网。这种宿主改用 nftables 表实现隔离，参照无虚拟化版安装脚本写入的 `/etc/cage-tenant/guard.nft`：转发链丢弃私网、组播与 IPv6，输入链只放行来自网桥的 DHCP、DNS 和 `ct status dnat` 连接。控制端口继续只监听 VM 回环，宿主防火墙仅向浏览器开放 TLS 入口。Incus 管理 socket 的权限相当于宿主管理员权限，运行网关的专用系统账户和部署配置属于宿主信任边界。

工作台页面和响应由该 VM 控制。网关按持久化记录选择目标，只连接对应 VM 内的回环端口。工作台内部会话由网关持有，浏览器只接收门户和对应 VM 源的 Cookie。部署时将工作台响应视作租户内容。

## 2. 制作空白 VM 镜像

1. 在独立构建项目中从已验证的 Incus Linux **VM** 基础镜像启动一次性构建机。选择已安装并启用 Incus guest agent 的基础镜像。在 `cage-image-build` 项目导入验证过的 `debian-12-vm` 后执行 `incus --project cage-image-build launch local:debian-12-vm cage-template --vm`。为 Ubuntu 24.04 单独制作模板并使用独立镜像别名。
2. 在构建机中安装 Super Cage Linux 服务端、所选工作环境的依赖、`socat` 和 `coreutils`。确认 `/usr/bin/socat` 与 `/usr/bin/head` 存在。以普通 `cage` 系统账户运行 `cage serve`，使用独立配置目录。`CLAUDE_CAGE_CONFIG=/home/cage/.claude-cage/config.json` 对应默认的 `-guest-control-path`。可用专用 systemd 用户服务和 `loginctl enable-linger cage` 确保无需登录即可开机启动。宿主提供嵌套虚拟化时，VM 内的 Super Cage 用本机模式运行自己的客机：再安装 `qemu-system-x86`（arm64 为 `qemu-system-arm`）、`qemu-utils`、`ovmf`、`openssh-client`、`iproute2` 和 `git`，把 `cage` 用户加入 `kvm` 组，并把 Linux 安装包 `/usr/lib/claude-cage/` 中的 `cage` 与 `mihomo` 放在同一目录（代理核心只在 `cage` 旁边查找）。`images:debian/12/cloud` 等 cloud 镜像会由 cloud-init 创建 UID 1000 的默认用户，建模板前删除该用户，并写入 `users: []` 禁止新 VM 首次启动时再建。
3. 如采用 Super Cage 云工作环境，按云端文档准备当前版本的完整工作环境模板、rootless 引擎与用户服务。将经过校验的模板预载进**构建 VM 内**的 rootless 引擎，并用 `cage init` 和 `cage container-configure --json` 写入空白运行配置。使用 `deploy/cloud/cage-container-engine.service` 与 `deploy/cloud/cage-server.service`，依赖、镜像与 socket 都留在每台租户 VM 内。仅复制空白模板层，让新 VM 自行生成工作台、浏览器和工作环境身份。[Super Cage 云端构建说明](../cloud/README.md)
4. 在一次性构建机停止 Super Cage 服务和引擎。按发行版镜像制作步骤清理构建机身份、SSH 密钥和构建日志。检查 Super Cage 配置和引擎镜像层，确保镜像内没有工作台账户或保险箱、项目、历史、`control.json`、会话、模型或代理凭据，以及任何用户数据。保持开机服务与 guest agent 启用后停止构建 VM。

将空白 VM 镜像收录到专用项目。生产启动命令可选择项目内通用别名或固定指纹。

```sh
incus --force-local --project cage-image-build stop cage-template
incus --force-local --project cage-image-build publish cage-template --alias cage-blank-build
image_dir=$(mktemp -d)
incus --force-local --project cage-image-build image export \
  local:cage-blank-build "$image_dir/cage-blank"
incus --force-local --project cage-tenant image import \
  "$image_dir/cage-blank.tar.gz" --alias cage-blank
incus --force-local --project cage-tenant image info local:cage-blank
```

本机导出和导入不要求 Incus 管理 API 在网络上监听。上述文件名适用于 `incus publish` 的默认压缩格式；如果改了格式，请使用实际导出的文件名。为每个不同系统重复此步骤。

多系统部署使用 `-systems-file /etc/cage-tenant/systems.json`。文件由宿主管理员维护，每条条目绑定该项目内的完整镜像指纹。将下面两个示例值分别换为验证过的 Debian 和 Ubuntu 镜像指纹。

```json
[
  {"id":"debian-12","label":"Debian 12","image":"1111111111111111111111111111111111111111111111111111111111111111"},
  {"id":"ubuntu-2404","label":"Ubuntu 24.04","image":"2222222222222222222222222222222222222222222222222222222222222222"}
]
```

门户和工作台按名称创建时，使用目录第一项作为默认系统。每台 VM 保存创建时的系统 ID、显示名和完整镜像指纹。可以在目录下架系统并保留已配备 VM。尚未配备的记录使用保存的指纹，镜像失效时可在恢复原指纹后重试。单系统部署使用 `-incus-image`，对应 `debian-12`。生产建议传完整指纹。本机别名在网关启动时解析为固定指纹。配置 `-systems-file` 或 `-incus-image` 后才能创建新虚拟机。已有实例可用原记录继续打开。

从专用项目启动一次性预检 VM，确认 agent、Super Cage 监听与镜像隔离。预检只打印是否通过，不打印控制文件、请求体或任何令牌。在示例项目的配额里为预检 VM 留一个位置，预检结束后按运维流程回收它。

```sh
incus --force-local --project cage-tenant launch local:cage-blank cage-preflight --vm --profile default
incus --force-local --project cage-tenant exec cage-preflight --mode=non-interactive -- \
  /usr/bin/test -s /home/cage/.claude-cage/control.json
incus --force-local --project cage-tenant exec cage-preflight --mode=non-interactive -- \
  /usr/bin/ss -ltn
incus --force-local --project cage-tenant config show cage-preflight --expanded
```

首次引导时，待控制服务就绪再重试预检。`ss` 的 Super Cage 端口应位于 `127.0.0.1`。用两个独立预检租户确认实际项目、各自的 VM 根盘、网段隔离和独立账户。关停再启动其中一台后，验证测试项目文件仍存在。验证过程使用本地测试文件，保持模型调用关闭。

## 3. 构建并启动网关

服务端归档 `Super-Cage-Server-*-linux-<架构>.tar.gz`（v0.1.14 及更早为 `Cage-Tenant-Server-*`） 包含 `cage-tenant`、Web 门户、本文和 API 契约，不包含 Incus、反代或 VM 镜像。下载与宿主架构匹配的归档，校验发布页的 SHA-256，再在独立部署目录解压。在干净的 Git 工作区运行 `bash deploy/tenant/package-server.sh` 可在 `_output/` 构建 amd64 和 arm64 归档，打包时会校验二进制记录的提交号；仅需本机编译时，在仓库根执行 `go build -o ./cage-tenant ./cmd/cage-tenant`。

以下从包含 `cage-tenant` 和 `ui/tenant` 的目录启动。`ui/tenant` 只需 `index.html`、`app.mjs` 和 `styles.css`。使用可信部署目录里的文件，并确保运行账户可写的状态目录权限为 `0700`。

```sh
./cage-tenant \
  -listen 127.0.0.1:8080 -origin https://cage.example.org \
  -state-dir /var/lib/cage-tenant -ui-dir ./ui/tenant \
  -incus-bin /usr/bin/incus -incus-project cage-tenant -incus-profile default -incus-image cage-blank \
  -guest-control-path /home/cage/.claude-cage/control.json \
  -account-vm-limit 5 -total-vm-limit 20 -parallel-starts 2 \
  -trust-loopback-client-ip
```

空闲冷却默认开启（`-freeze-after 15m`、`-stop-after 72h`），参数和前提见第 7 节。VM 模式默认开启弹性内存（`-memory-interval 15s`），参数见第 9 节；现有启动命令和 systemd 单元无需修改。

托管工作台账户默认启用。首次启动时，网关在状态目录创建私有文件 `guest-account.json`（`0600`），内容为 `{"username":"cage","secret":"<64 位小写十六进制>"}`；每个 VM 的密码由该密钥和 VM ID 派生，不写入 `state.json` 或浏览器。需要显式指定或与其它主机共享同一密钥时，可加 `-guest-account-file /etc/cage-tenant/guest-account.json`，文件必须为 `0600` 的同一 JSON 格式。更改或丢失密钥后，已有托管账户不会被改写或删除，但会回退到工作台自己的密码界面；需要保留自动进入时，应随状态目录一起备份该文件。

将同一条启动命令配置为宿主 systemd 服务，使用专用账户、固定 `WorkingDirectory`、`StateDirectory=cage-tenant`、`StateDirectoryMode=0700` 和 `Restart=on-failure`，并在本机 Incus 服务就绪后启动。为 Incus CLI 配置可信的本机 Unix socket 访问。预制镜像和有效项目 profile 都由宿主管理员维护。

**首批门户账号：** 生产默认关闭注册。先在私有 VPN 或有身份认证的 TLS 入口限制门户的 `POST /api/tenant/register`，确认未获准用户无法抵达该路径，再临时加 `-enable-preview-registration`。让首批获准用户在该入口自己注册并保管密码；随后移除该参数并重启，已有账户仍可登录，重启前会话需重新登录。预览版没有邀请系统或账户管理 CLI；不要为了注册修改状态文件，也不要在匿名公网入口开启此开关。所有 VM 记录（包括未配备、失败和停机）都占配额；开放之前先按获准人数与宿主容量核对每账户和全局限额。当前没有自动回收或归档流程。

本地开发运行 `./_output/cage-tenant -dev -listen 127.0.0.1:8080 -state-dir /path/to/private/dev-state -incus-image cage-blank` 后访问 `http://cage.localhost:8080`。浏览器需将 `cage.localhost` 和 `vm-<id>.cage.localhost` 解析到回环。HTTP 开发使用单独的 Cookie 名称。实际打开 VM 仍需本机 Linux Incus。

## 4. 配置 DNS、TLS 和反代

将 `cage.example.org` 与 `*.cage.example.org` 的 DNS 指向同一 TLS 代理。证书应覆盖这两个名字。对外域名、`-origin`、证书及客户端使用的端口须一致。反代保留原始 Host 和 Origin，用直接客户端地址覆盖 `X-Real-IP`，支持 WebSocket 并关闭响应缓冲。开启 `-trust-loopback-client-ip` 时，只有可信 TLS 代理可以直连网关。

向用户提供 HTTPS 门户地址。用户直接在浏览器登录，创建或打开第一台 VM。门户随后加载该 VM 的 Super Cage 工作台，用户可从左栏 **虚拟机** 创建和切换。原生桌面版使用同一个工作台组件管理本机虚拟机。服务器清单归门户账户，本机清单归设备上的 Super Cage 账户。

Nginx 示例。`map` 位于 `http` 块，其他虚拟主机使用各自配置。生产环境请避免记录请求体、Cookie、来源票据、查询字符串及工作台内容。

```nginx
map $http_upgrade $cage_connection {
    default upgrade;
    ''      close;
}

server {
    listen 443 ssl;
    http2 on;
    server_name cage.example.org *.cage.example.org;
    ssl_certificate     /etc/nginx/tls/cage-fullchain.pem;
    ssl_certificate_key /etc/nginx/tls/cage-key.pem;
    access_log off;
    error_log /dev/null;
    client_max_body_size 2050m;
    client_body_timeout 30s;

    location ^~ /api/tenant/ {
        client_max_body_size 4k;
        client_body_timeout 5s;
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_request_buffering off;
        proxy_buffering off;
        proxy_cache off;
    }

    location ^~ /assets/ {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_request_buffering off;
        proxy_buffering off;
        proxy_cache off;
        gzip on;
        gzip_vary on;
        gzip_min_length 1k;
        gzip_comp_level 5;
        gzip_types text/css text/javascript application/javascript image/svg+xml;
    }

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $cage_connection;
        proxy_request_buffering off;
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 1h;
        proxy_send_timeout 1h;
        send_timeout 1h;
    }
}
```

**HTTP/2。** 示例里的 `http2 on;` 需要 nginx 1.25.1 或更新；更早的版本把它写进监听行：`listen 443 ssl http2;`（新版本里这种写法已弃用）。HTTP/1.1 下浏览器对每个主机只开 6 条连接，工作台的浏览器事件流（EventSource）会一直占着其中一条，其余请求在剩下的 5 条上排队；HTTP/2 把它们放进同一条连接的多个流。WebSocket（终端、画面）仍用 HTTP/1.1：nginx 不提供 RFC 8441 的 HTTP/2 WebSocket，浏览器会为它们另开 HTTP/1.1 连接。到网关的上游仍是 HTTP/1.1。验证：DevTools 的 Network 面板打开 Protocol 列，工作台请求应显示 `h2`；`curl -sI --http2 https://cage.example.org/ | head -1` 应以 `HTTP/2` 开头。

**静态资源。** 对 `/api/` 之外的 GET 和 HEAD，状态为 200、206 或 304 的响应，网关转发工作台自己的缓存头：`Cache-Control`、`ETag`、`Last-Modified`、`Vary`，`Content-Encoding`、`Accept-Ranges` 和 `Content-Range` 本来就转发。网关预先设置的 `no-store` 会先去掉，否则两个值都会发出，`no-store` 仍然生效。带哈希的资源因此可以被浏览器缓存一年，HTML 用 `ETag` 校验、未变时只花一个 `304`。`public` 被改成 `private`：门户用 Cookie 鉴权，共享缓存不认识它，不能存这些响应。生存期最多一年，不认识的指令和 `no-store` 一律保持 `no-store`；API、错误和网关自己的页面始终是 `no-store`。旧版工作台只发 `no-store`，此时行为不变，缓存要等工作台给资源发缓存头才生效。浏览器的 `Accept-Encoding` 原样传给工作台，所以工作台预压缩的 br 或 gzip 资源按原字节传到浏览器；浏览器没发 `Accept-Encoding` 时网关向工作台要不压缩的内容。工作台自己不压缩时，由上面 `/assets/` 位置的 nginx `gzip` 压缩；该位置只压缩样式、脚本和 SVG，已带 `Content-Encoding` 的响应 nginx 不再处理，其余头（`Cache-Control`、`ETag`、`Vary`、`Accept-Ranges`）nginx 默认原样转发，对压缩后的响应它把强 `ETag` 改成弱 `ETag`，浏览器的校验仍然有效。

终端、浏览器画面和事件流是长连接。工作台每 20 秒在终端连接上发送心跳，并在客户端 90 秒无应答后释放连接；客户端在约 50 秒收不到任何数据时自行重连并从断点续传。沿途每一跳的空闲期限都应明显长于心跳间隔，上面三个 1 小时的超时即为此设置；省略时 nginx 默认 60 秒，网络稍有阻塞就会断开写入中的连接。

如果公网入口是另一台机器上的 nginx `stream`（例如经 `proxy_protocol` 和 SSH 反向隧道转发到本机 nginx），在对应 `server` 中设置 `proxy_timeout 1h;`（默认 10 分钟），并为隧道开启保活，避免中间的 NAT 或防火墙静默丢弃连接：

```sh
ssh -N -o ServerAliveInterval=10 -o ServerAliveCountMax=3 -o ExitOnForwardFailure=yes \
    -R 127.0.0.1:9443:127.0.0.1:443 relay.example.org
```

隧道断开时 `ssh` 会退出，应由 systemd 或 autossh 负责重启。

工作台反代上限为 2050 MiB，给单文件 2 GiB 上限留出请求封装余量；更大的整个请求仍可能得到 `413`。`client_body_timeout` 限制两次读取之间的空闲时间，不限制正常持续上传的总时长。网关另外限制小 JSON 的整个读取时间；会话撤销会尝试中断网关与下游之间的读写，已提交到来宾的修改或反代已收下的数据无法撤回。

## 5. 重启、配额和数据保留

- 网关重启保留账户、VM 归属和记录。登录会话、VM Cookie 和 60 秒单次票据都保存在进程内。重启后重新登录，首次打开重新探测原 VM，页面在探测前显示 `stopped`。网关退出、登出和切换页面不会停止 Incus 实例。
- 工作台里的停止操作管理 VM 内的运行环境。停止外层 VM 由管理员在专用项目执行 `incus --project cage-tenant stop cage-<id>`。再次打开会启动原 VM，沿用其根盘。宿主重启后的 VM 自启动行为可在专用 Incus profile 配置 `boot.autostart`。
- 账户配额和全局配额计入所有持久 VM 记录，包含停机的 VM。`-parallel-starts` 只限制网关任务；Incus 守护进程的异步配备或启动可能在 CLI 超时后继续。超时后先用 `incus --force-local --project cage-tenant operation list` 和实例状态对账，再手动重试。对同一 VM 的请求加入网关已有任务，排队时状态保持 `starting`。网关按第 7 节自动冻结和停止空闲实例；管理员也可手动停止外层 VM，磁盘保持原样。宿主配置应关闭跨租户内存页合并，例如 KSM。更新系统目录里的镜像指纹只影响之后创建的 VM 记录。
- 网关确认上游连接故障或内部会话失效后，清除该工作台就绪缓存和未使用票据；从门户重新打开可触发探测及必要时重新握手。网关不重放写请求；如果响应中断，先检查上次操作是否已在来宾完成，再决定是否重试。工作台的嵌套 SSH 模式当前不受支持，普通业务 API 在切换后可能返回 `409`，请保留 VM 本地工作环境模式。
- 按同一恢复点备份 `/var/lib/cage-tenant` 与 Incus 实例、项目、存储池和相关镜像。`state.json` 保存密码哈希和 VM 所有权，`guest-account.json` 保存托管工作台账户的派生密钥。回滚或迁移前停网关并保管备份，保留 VM ID、实例名及 `user.cage-tenant.id` 绑定。一个状态目录只运行一个网关进程。
- v1 有 VM 记录时，必须用 `-legacy-debian-image <64 位原始指纹>` 明确迁移；不从当前系统目录、别名或替代镜像推断。指纹应来自可信旧部署档案或原实例的 `volatile.base_image`（并核实确为旧 Debian 模板）。已经迁移成空指纹的旧 v2 Debian 记录可用同一参数补绑，不覆盖任何非空绑定；未补绑时，已有实例经过校验仍可打开，未配备的记录不能创建 VM。无法确认原镜像时保留备份，不猜测指纹。
- VM 就绪和错误状态只存在于内存，不依赖状态文件写入。账户或 VM 记录写入若在原子替换前失败，修复存储后可重试；替换后若目录同步失败，网关保守阻止新登录和打开，先检查宿主存储和持久化状态，再重启。
- 验收使用两个独立门户账户，每个创建至少两台 VM。在同一门户切换，核对不同工作台数据，再以第二个账户确认第一个账户的 VM 不可访问。门户登出会撤销该次登录发放的票据、HTTP 流和 WebSocket。

## 6. 检查根盘复用与运行成本

同一门户的所有账号共用管理员维护的空白镜像目录。复用以完整镜像指纹为准，配备时所有账号使用同一个 Incus 项目、profile 和存储池。每台 VM 使用独立可写根盘，首次启动后生成的账号数据、项目文件和凭据均保存在该 VM 内。

让 ZFS 镜像 zvol 的 `volsize`、Incus 池的 `volume.size` 和 profile 根盘 `size` 一致，并确认模板镜像虚拟盘大小在该容量内。用两个不同门户账号各创建并打开一台使用同一镜像指纹的 VM，再检查它们的根盘。用其中一台写入测试文件，确认另一台读取不到该文件，并检查两个账号的清单各自只显示所属 VM。

```sh
incus --force-local storage show tenant-pool
incus --force-local storage info tenant-pool
incus --force-local --project cage-tenant storage volume list tenant-pool
sudo zfs list -t volume -o name,volsize,used,refer,origin,refreservation -r cage-zfs
```

同镜像的两个 VM 根盘应各自持有可写 zvol，并在 `ORIGIN` 显示同一镜像的 `@readonly` 快照。`VOLSIZE` 是来宾看到的标称虚拟容量，`REFER` 包含从快照继承的块，`USED` 还可能计入保留量。测试环境中，Incus 6.0、统一 10 GiB 的 ZFS 镜像和根盘获得了共同的 `@readonly` 来源。根盘容量不同的 ZFS 实例和同环境的 Btrfs 实例获得独占块。使用其他驱动、容量或 Incus 版本时，按现场测量安排磁盘预算。

在预检浏览器的 Network 面板记录第一次 `open` 到列表显示 `ready` 的时间。切换到已运行 VM 时，记录 `open` 至工作台首个文档响应的时间。只记耗时和状态码，避免导出含认证请求的 HAR。配备、切换和停止 VM 前后执行以下命令，对比 Incus 资源、进程数量与宿主可用内存。

```sh
incus --force-local --project cage-tenant info cage-preflight
incus --force-local --project cage-tenant list
free -h
ps -C qemu-system-x86_64 -o pid,rss,%cpu,comm
ps -C incus -o comm= | wc -l
```

每台 VM 独立复用一个 Super Cage 会话和 HTTP 连接池，最多保留 16 条空闲 guest-agent 连接，空闲 45 秒后关闭。工作台自己在连接空闲 60 秒后关闭它，网关先放手，被复用的连接就很少恰好是来宾正在关闭的那一条。即便如此，复用的空闲连接上的请求失败时，网关先用健康检查确认控制器，检查通过就保持就绪；新建连接上的失败仍立即清除就绪状态。

同一 VM 的普通 HTTP 请求最多 64 条同时进行；WebSocket 升级请求和 `Accept: text/event-stream` 的事件流另有 128 条的预算，因此打开很多终端、画面和事件流不会占满普通请求的名额，状态轮询和点击不会因此得到 `429`。任一预算用尽时，只有同类的新请求收到 `429`。另保留两条连接用于握手和健康检查。每条流在宿主上占一个 `incus exec` 与 `socat` 进程，所以 128 是一台 VM 的宿主进程上限，不是推荐值。长连接由正常的读写回压控制，登录撤销会关闭该会话正在使用的流和 WebSocket。

## 7. 空闲冷却

网关参考 [Dormice](https://github.com/BitMiracle-AI/Dormice) 的分级冷却：空闲实例每轮扫描最多降一级，任何使用都会把它带回运行状态。

```text
运行 ── 空闲 ──▶ 冻结 ── 空闲 ──▶ 停机
全部内存         进程保留，内存换出到 swap    只剩磁盘
                 下一个请求约 0.1 秒内恢复    从门户打开时冷启动
```

- `-freeze-after`（默认 `15m`，`0` 关闭冷却，最短 `1m`）：空闲达到该时长后执行 `incus pause`，再把实例 cgroup 的当前内存用量写入 `memory.reclaim`，把内存换出到 swap。冻结前网关断开该实例已转发的请求和长连接（WebSocket、事件流）。下一个工作台请求先恢复实例再转发，浏览器不需要重新登录，工作台会自动重连。
- `-stop-after`（默认 `72h`，`0` 表示一直保持冻结）：使用同一个空闲时钟。冻结的实例先恢复再正常关机，60 秒后仍未关机则强制停止，磁盘保留。门户随后显示 `stopped`，再次打开即冷启动。
- `-idle-cpu-percent`（默认 `5`，单位为单核的百分比，`0` 表示不看来宾 CPU）：实例自身的 CPU 用量超过该比例时视为活动，没有浏览器连接时仍在工作的代理任务不会被冻结。实测空闲的 Super Cage 控制服务约占单核的 0.1%。
- 门户「打开」、经网关的每个请求，以及请求和流在任一方向上传输的数据都计入活动。工作台页面开着时会定期轮询，实例保持运行；关闭页面或断网后才开始计时。
- 扫描间隔为 `-freeze-after` 的四分之一，最短 5 秒、最长 1 分钟。每轮用一次 Incus 查询读取全部实例。冷却状态只保存在内存中：网关重启后重新计时，之前被冻结的实例在下次打开时恢复。

前提与注意事项：

- 宿主需要 swap，容量约等于需要同时冻结的内存。容器的匿名内存在 `vm.swappiness=60` 时即可换出；部分云镜像默认 `vm.swappiness=0`，请用 `sysctl vm.swappiness` 确认实际生效值。
- 主动换出需要写 Incus cgroup 的 `memory.reclaim`：以 root 运行网关，或在 systemd 服务中设置 `AmbientCapabilities=CAP_DAC_OVERRIDE`。Incus 管理 socket 本身已等同宿主管理员权限。缺少权限时网关只记录一次日志并跳过；冻结实例的内存会在宿主内存紧张时由内核换出。
- 写入在子进程中执行，30 秒后强制结束。请求量超过可回收量时内核返回 `EAGAIN`，属于正常情况。
- 容器模式的 profile 必须设置 `limits.memory.swap=<大小>`。设置了 `limits.memory` 后，Incus 6.0 会把容器的 `memory.swap.max` 设为 0，只写 `true` 并不会放开 swap。
- 不要对冻结的实例直接执行 `incus stop`。冻结的实例收不到关机信号；实测 Incus 6.0.0 在停止失败后把仍处于冻结的实例报告为 Running，随后的 `incus exec` 会一直挂起。应先 `incus start`（即恢复）再停止，网关自己的停机也按这个顺序执行。
- VM 模式下冻结会暂停 QEMU 的 vCPU。本轮没有在 KVM 宿主上实测 VM 的内存换出；如果 QEMU 进程不在该实例自己的 Incus cgroup 中，网关会跳过换出并记录日志。

实测环境：Lima arm64 虚拟机，无 `/dev/kvm`，Ubuntu 24.04，Incus 6.0.0 与 6.0.7；4 个容器租户，每个只运行空白模板里的 Super Cage 控制服务。

| 项目 | 结果 |
| --- | --- |
| 运行中每个租户的常驻内存 | 约 100 MiB |
| 冻结后每个租户的常驻内存 | 3–16 MiB，其余约 90 MiB 在 swap |
| 4 个租户全部冻结后宿主已用内存 | 1074 MiB → 683 MiB |
| 冻结状态下第一个工作台请求（含恢复） | 71–139 ms，之后的请求 2–3 ms |
| 恢复后换回内存的量 | 约 7 MiB |
| 停机后从门户重新打开 | 2.3–2.5 秒就绪 |
| 一个租户内跑满一个 CPU | 该租户保持运行，其余 3 个按时冻结 |

## 8. 无硬件虚拟化的宿主（Incus 系统容器）

无虚拟化版安装包（`Super-Cage-NoVirt-<版本>-linux-<架构>.tar.gz`）中的 `install.sh` 会自动完成本节的全部步骤，见[无虚拟化版说明](novirt/README.md)。下面是手动配置的说明。

没有 KVM（`/dev/kvm` 不存在，例如云主机未开嵌套虚拟化）时，用 `-incus-instance-type container` 启动网关。每个工作区成为专用项目中的 Incus 无特权系统容器 `cage-<id>`，网关的其余行为不变：仍通过 `user.cage-tenant.id` 标签认领实例，仍经 `incus exec` 和 `socat` 连接实例内回环监听的 Super Cage 控制端口。同一个项目只能使用一种实例类型，`-incus-image` 和 `-systems-file` 也只接受该类型的镜像。

**隔离差异。** 容器与宿主共用内核，隔离弱于虚拟机。请使用专用宿主并及时更新内核。网关拒绝 `security.privileged`、全部 `raw.*`、`linux.kernel_modules` 等键，不允许 profile 增加根盘和受管 NIC 以外的设备，并要求实例的资源与隔离设置与 profile 完全一致。需要更强隔离又没有 KVM 时，可以考虑 Dormice 使用的 gVisor（用户态内核），本仓库尚未实现。

**宿主。**

- 推荐 [Zabbly](https://github.com/zabbly/incus) 的 Incus 6.0 LTS（实测 6.0.7）。Ubuntu 24.04 自带的 6.0.0 能运行网关和 Super Cage 控制服务，但租户容器内的 rootless 引擎创建容器时会报 `open sysctl net.ipv4.ip_unprivileged_port_start file: reopen fd 8: permission denied`（runc 1.3.3 的 CVE-2025-52881 修复与旧版 LXC 不兼容，见 runc #4968）。同一宿主换成 6.0.7 并重启容器后可以正常创建。从发行版包换成 Zabbly 包时，现有实例会被停止，请安排在维护窗口。
- 按第 7 节开启 swap。
- 调高 inotify 上限，例如 `fs.inotify.max_user_instances=1048576` 和 `fs.inotify.max_user_watches=1048576`。无特权容器的 root 映射到宿主的同一个 uid，所有容器共用这个 uid 的 inotify 实例上限，systemd 每个容器要占几个，发行版默认的 128 只够少量容器启动，超出后容器内 systemd 报 `Failed to create control group inotify object: Too many open files` 并退出。
- Ubuntu 23.10 起默认 `kernel.apparmor_restrict_unprivileged_userns=1`，会阻止容器内的 rootless 引擎创建用户命名空间（日志为 `rootlesskit ... fork/exec /proc/self/exe: permission denied`）。使用 Super Cage 云工作环境时，在 `/etc/sysctl.d/` 中设为 `0` 并持久化；升级 AppArmor 或 Incus 包后需复查。这会允许宿主上所有普通用户创建无特权用户命名空间，只应在专用宿主上这样设置。Debian 宿主没有这项限制。
- Incus 默认为无特权容器提供 `/dev/fuse` 和 `/dev/net/tun`，并分配足够的 uid/gid 映射，Super Cage 云工作环境所需的 FUSE、TUN 和嵌套用户命名空间都不需要额外设备。

**项目与 profile。** 受限项目中，`limits.memory.swap` 属于低级配置；Incus 6.0 检查 profile 时使用的是 `restricted.virtual-machines.lowlevel`，因此两项都要放开。放开后，`raw.*` 等键仍由网关拒绝，VM 配额为 0。

```sh
incus --force-local project create cage-tenant \
  -c features.images=true -c features.profiles=true \
  -c restricted=true -c restricted.devices.disk=managed -c restricted.devices.nic=block \
  -c restricted.containers.nesting=allow \
  -c restricted.containers.lowlevel=allow -c restricted.virtual-machines.lowlevel=allow \
  -c limits.containers=20 -c limits.virtual-machines=0 \
  -c limits.cpu=80 -c limits.memory=160GiB -c limits.disk=1TiB
incus --force-local --project cage-tenant profile device add default root disk \
  path=/ pool=tenant-pool size=10GiB
incus --force-local --project cage-tenant profile set default \
  limits.cpu=4 limits.memory=8GiB limits.memory.swap=8GiB limits.processes=16384 \
  security.nesting=true
```

`security.nesting` 让容器内可以运行 Super Cage 的 rootless 引擎；`limits.processes` 限制任务数；`limits.memory.swap` 见第 7 节。以上 profile 已在测试机上实测。容器模式下网关还接受 `security.idmap.isolated`、`security.idmap.size`、`security.syscalls.intercept.mknod`、`security.syscalls.intercept.setxattr` 和 `limits.memory.swap.priority`，但 `security.idmap.isolated` 未验证通过：测试机上 Incus 为实例分配的独立区间不在 root 的 `/etc/subuid` 范围内，`newuidmap` 拒绝映射，实例无法启动；默认的 65536 个 ID 也容不下容器内 rootless 引擎需要的从属 ID。启用前先核对 root 的 subuid/subgid 区间，并把 `security.idmap.size` 设到 131072 以上，然后实测。

**空白容器镜像。** 步骤与第 2 节相同，区别是从容器镜像启动构建实例，并用系统服务运行 `cage serve`。构建项目需要一个受管网络用于安装软件包，构建网络不会进入发布的镜像。

```sh
incus --force-local --project cage-image-build launch images:debian/12 cage-template
incus --force-local --project cage-image-build exec cage-template -- sh -c \
  'apt-get update && apt-get install -y --no-install-recommends socat ca-certificates && useradd --create-home --uid 1000 --shell /bin/bash cage'
incus --force-local --project cage-image-build file push ./cage cage-template/usr/local/bin/cage --mode 0755 --uid 0 --gid 0
incus --force-local --project cage-image-build file push ./cage-serve.service cage-template/etc/systemd/system/cage-serve.service
incus --force-local --project cage-image-build exec cage-template -- systemctl enable cage-serve.service
```

其中 `cage-serve.service` 为：

```ini
[Unit]
Description=Super Cage control service
After=network.target

[Service]
User=cage
Environment=CLAUDE_CAGE_CONFIG=/home/cage/.claude-cage/config.json
Environment=HOME=/home/cage
WorkingDirectory=/home/cage
ExecStart=/usr/local/bin/cage serve
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
```

采用 Super Cage 云工作环境时，按[云端构建说明](../cloud/README.md)在构建容器内为 `cage` 用户准备 rootless 引擎、用户服务和 `loginctl enable-linger cage`。发布前按第 2 节第 4 步停止服务并清理身份（至少删除 `/home/cage/.claude-cage`、SSH host keys 和日志，清空 `/etc/machine-id`），然后 `incus publish`、导出并导入 `cage-tenant` 项目。

**启动网关。** 在第 3 节的命令上加 `-incus-instance-type container`，`-incus-image` 指向容器镜像。

**本轮验证范围。** 在无 `/dev/kvm` 的 Ubuntu 24.04 arm64 虚拟机上：网关以容器模式创建、认领、连接 4 个租户（每个约 5 秒就绪，冷启动约 2.5 秒），经网关访问工作台，完成第 7 节的冻结、恢复、停机和重开。在 `security.nesting=true` 的无特权容器内，rootless Docker 29.0.0（runc 1.3.3）以 `--network none`、内存和进程上限运行容器，前提是 Incus 6.0.7 且宿主放开了上面的 AppArmor 限制。完整的 Super Cage 云工作环境镜像（仅支持 amd64）和真实模型调用未在容器租户中验证。

## 9. 弹性内存（VM 模式）

VM 模式下，profile 的 `limits.memory` 是每台 VM 的内存上限，不是固定分配。网关按宿主当前的内存，在运行中调整每台 VM 的实例级 `limits.memory`。Incus 通过 virtio balloon 在线生效，VM 不重启。容器模式不调整：容器只占用实际用到的内存，其 `limits.memory` 仍须与 profile 一致。

**启动在上限。** 停机的 VM 启动前，网关删除它的实例级 `limits.memory`，让它按 profile 上限启动。之后的调整都不超过启动时的内存，Incus 只移动 balloon，不热插内存（热插的内存在下次启动前收不回来）。网关的空闲停机之后也会删除该值。不经网关启动的 VM，例如宿主重启后由 Incus 自启动、或来宾在 VM 内自行重启，按当时的实例级大小启动，见下面的失败处理。

**每轮做什么。** 每轮间隔为 `-memory-interval`（默认 `15s`）。网关读取宿主的 `/proc/meminfo` 和 `/proc/pressure/memory`、profile 上限，并经 `incus exec` 用 `/usr/bin/head` 读取每台运行中 VM 的 `/proc/meminfo`。来宾工作集按 `MemTotal − MemAvailable` 计算，不含页缓存。Incus 自己报告的 VM 内存用量是 `MemTotal − MemFree`，包含页缓存，不能用作下限。

- **需要量**：工作集加余量（1 GiB 与工作集四分之一中的较大者），再加来宾内核占用，向上取整到 256 MiB，且不低于 `-memory-min`。
- **立即增长**：VM 少于需要量时，下一轮直接增长到需要量，宿主紧张时也一样，因为来宾内 OOM 比宿主换页更糟。增长不超过 profile 上限，也不超过「宿主内存 − 预留」。
- **宿主紧张时回收**：宿主 `MemAvailable` 低于预留的两倍，或 PSI `some avg10` 达到 10% 时，每台 VM 每轮最多缩小 512 MiB，直到需要量为止。回收的主要是来宾页缓存。
- **公平份额**：份额 = (宿主内存 − 预留 − 冻结及本轮不调整的 VM 内存) / 可调整的运行中 VM 数。超过份额的 VM 每轮最多缩小 512 MiB，但不低于需要量。最近 1 分钟内有使用的工作区（经网关的请求和流数据、门户「打开」，以及开着空闲冷却时来宾 CPU 超过 `-idle-cpu-percent`）在宿主不紧张时不参与公平份额缩小，只有空闲的 VM 让出；宿主紧张时（上一条）所有 VM 照常缩小。例如第二台 VM 启动后，空闲的那台逐步让出内存；两台都在使用时，要等宿主可用内存低于预留的两倍才开始缩小。「在使用」按第 7 节的活动判断：工作台页面开着时会定期轮询，所以开着页面的 VM 都算在使用，公平份额缩小实际只作用于没有页面开着、也没有来宾 CPU 活动的 VM；来宾 CPU 活动由空闲冷却的扫描记录，最长每分钟更新一次，与 1 分钟的窗口相当，边缘上可能差一轮。这样更安全：宿主一旦紧张，所有 VM 仍照常缩小，工作集和余量不受影响，被回收的只是页缓存。
- **其余情况保持不变**：宿主有空闲内存本身不会让 VM 增长，所以缩小后不会自己弹回。差距不到 512 MiB 时不调整。
- **不调整的 VM**：冻结、停机、正在准备或正在冷却的 VM 不调整；网关调整某台 VM 时，空闲冷却先等它完成。
- **来宾不应答**：读取来宾 `/proc/meminfo` 失败的 VM 本轮不调整。连续两轮失败时，它可能正因内存不足而卡顿，网关把它恢复到上限（宿主紧张时每轮增长 512 MiB）。

**失败处理。** 来宾 5 秒内没有到达目标时，Incus 报 `taking too long`。调整失败只记录网关日志，工作台保持就绪；缩小失败后，该 VM 一分钟内不再缩小。每次在线修改，Incus 都会设置 `volatile.vm.needs_reset`：来宾在 VM 内自行重启时，Incus 会完整重启 QEMU，按当时的实例级大小启动。这样的 VM 之后需要增长时，Incus 会热插内存。`volatile.vm.hotplug.memory` 不为空的 VM 在下次启动前只增长、不缩小。网关下次启动它时，会重新按上限启动。

**参数。**

- `-memory-interval`：默认 `15s`，`0` 关闭弹性内存，可设范围为 5 秒到 10 分钟。
- `-memory-reserve`：宿主自身保留的内存，默认取 2 GiB 与宿主内存 15% 中的较大者。宿主可用内存低于它的两倍时，开始回收来宾缓存。宿主上有 ZFS ARC 等不计入 `MemAvailable` 的大块占用时，应调大该值，例如 `-memory-reserve 4GiB`。
- `-memory-min`：给运行中 VM 的最小内存，默认 `2GiB`，不能低于 `1GiB`。

网关启动时会记录一行 `elastic VM memory every …`，每次调整也会记录一行，例如 `workspace <id> memory 10240 MiB -> 9728 MiB (host memory is low)`。

**算例。** 宿主 15 GiB，profile 上限 10 GiB，只有一台 VM 在运行。来宾 `MemTotal` 为 9890 MiB、`MemAvailable` 为 3783 MiB，宿主可用 3.1 GiB。预留是 2.25 GiB，宿主可用低于 4.5 GiB，算作紧张，VM 每 15 秒缩小 512 MiB：10 GiB → 9.5 → 9 → 8.5 GiB。宿主可用回到 4.5 GiB 以上时停止。如果宿主一直紧张，最低停在 8 GiB，即 6.0 GiB 工作集加余量和内核占用。

### 空闲页返还（可选）

Incus 6.0 创建的 virtio-balloon 设备没有开启 free page reporting，后续 Incus 版本默认开启。不开启时，来宾释放的内存要等 balloon 下次收缩才回到宿主。网关唯一接受的 `raw.qemu.conf` 是下面这一条 profile 级覆盖，它给 Incus 生成的 `device "qemu_balloon"` 加上 `free-page-reporting`：

```sh
# raw.qemu.conf 不能在 VM 运行时修改：先停网关，再停所有运行中的租户 VM。
incus --force-local project set cage-tenant restricted.virtual-machines.lowlevel=allow
incus --force-local --project cage-tenant profile set default \
  raw.qemu.conf="$(printf '[device "qemu_balloon"]\nfree-page-reporting = "on"')"
```

`restricted.virtual-machines.lowlevel=allow` 会让该项目接受所有低级 VM 配置，例如 `raw.qemu` 和 `raw.apparmor`。网关仍拒绝这条以外的全部 `raw.*`，但 Incus 本身不再拦截，项目里的这类限制只剩网关校验一道。只在只有网关能访问 Incus socket 时这样设置。设置后，VM 下次启动时生效。在 VM 内执行 `cut -c6 /sys/bus/virtio/drivers/virtio_balloon/virtio*/features`，输出 `1` 表示来宾已协商该特性（第 6 个字符对应特性位 5，即 `VIRTIO_BALLOON_F_REPORTING`）。这项设置本轮没有在 KVM 宿主上实测。

VM 内嵌套运行的 Super Cage 客机（QEMU 5.1 及以上；仅 Linux 宿主上的 QEMU）默认开启同一特性，嵌套客机释放的内存会先回到租户 VM，再由上面的设置或网关的 balloon 回到宿主。

## 10. 经 vsock 直连工作台（VM 模式，可选）

默认每条到工作台的连接都启动一个 `incus exec … socat`，字节经 incusd 和 guest agent 转发。网关每开一个 WebSocket 或一条新的池内连接，都要先付一次 exec 启动的时间，画面流量也经过 incusd 的 exec 中继。加 `-incus-vsock` 后，网关改为每个工作区经同一条 exec 启动一个常驻的中继 `/usr/local/bin/cage vsock-relay`，让它在 VM 的一个随机 vsock 端口上监听并转发到工作台的回环端口；之后网关直接连 VM 的 vsock 地址，新连接只是一次套接字连接加一次握手。该选项默认关闭，只用于虚拟机：容器与宿主共用内核和 vsock 命名空间，容器里的 vsock 监听会被其他容器连上，所以容器永远走 exec，`-incus-instance-type container` 加 `-incus-vsock` 会被拒绝启动。模板里的 `/usr/local/bin/cage` 必须是带 `vsock-relay` 的版本（本版及以后）；旧模板的 `cage` 不认这个子命令、立刻退出，网关就回到 exec。

**认证。** vsock 本身没有文件权限层，宿主上任何本地进程都能向 VM 的 vsock 端口发起连接，VM 里的进程也能在 vsock 上监听。所以网关每启动一个中继，就生成一个只属于这个中继的随机密钥，经 exec 的标准输入交给它（不进命令行、不进环境变量），标准输入在中继运行期间一直开着，网关退出或工作区关闭时中继随之退出。每条连接（包括池内复用的）在任何 HTTP 字节之前都要双向证明知道这个密钥：中继先发随机数，网关回自己的随机数和 HMAC，中继核对后才连工作台，再回自己的 HMAC，网关核对后才发请求。因此：

- 网关只向自己在本进程里启动并核对过的中继发请求和启动令牌；端口上别的监听（VM 里抢先占端口的进程、上一个网关进程留下的监听）拿不到任何请求字节，网关把该实例退回 exec 5 分钟并记日志。
- 中继只为证明了密钥的一方转发；在 Linux 上它还丢弃对端 CID 不是宿主（2）的连接，所以 VM 里的进程、它的内层沙箱和嵌套客机都到不了握手。宿主上的其他本地进程同样是 CID 2，挡住它们的是密钥。
- 两个方向的 HMAC 用不同标签、两端随机数都是新的，证明既不能反射也不能重放。

第 1 节「控制端口只监听回环」对 TCP 端口仍然成立；中继的 vsock 监听不经过任何网络。

**回退。** 直连只是优化，失败时用回今天的路径：

- 中继监听起来后，网关先经已认证的 vsock 连接发一个普通 HTTP 请求，确认到达工作台的 HTTP 服务；随后工作区握手（`/api/launch`、`/api/session`、`/api/client/about`）也走 vsock。
- 来宾侧的监听早于工作台启动。中继已在监听、但连不上工作台时，它用单独的标签回答「未就绪」，这次配备用 exec，不记为 vsock 的失败；中继随之结束，下一个工作区再试 vsock。
- 握手经 vsock 失败时，网关用同一个启动令牌改经 exec 再握手一次；只有 exec 成功、vsock 不成功，才认定是 vsock 的问题，该实例 5 分钟内不再尝试 vsock。
- 某次 vsock 连接失败时，在同一次拨号里改用 exec，请求和流看不到失败；单次失败不影响后面的连接（中继的监听队列是 128，同时在握手的连接最多 32 条）。连续 3 次失败后该工作区不再试 vsock，该实例 5 分钟内不再尝试。对方回应了却不是中继（问候不对，或证明不了密钥）时立刻放弃该路径；对方不出声或在握手中挂断（中继忙或慢时也会这样）只算一次普通的失败。
- 来宾的 `cage` 没有 `vsock-relay`（中继进程立刻退出）、监听 5 秒内没有出现、`volatile.vsock_id` 缺失，该实例 5 分钟内不再尝试。网关进程的 systemd 限制了 `AF_VSOCK` 时，每次拨号失败并改用 exec，连续 3 次后放弃。以上都只是回退，不影响工作区可用。

日志每个工作区一行：`dials its workbench through vsock (CID …, port …)` 表示在用直连；`uses incus exec; the vsock path is unavailable: …` 给出回退原因（`is not the bridge` 表示端口上的监听回应了却证明不了密钥，`nothing answered as the bridge` 表示 5 秒内没有监听完成握手）；`the vsock path ended (…)` 表示运行中退回 exec。回退不会让工作区报错，所以要靠这些日志确认直连真的在用。

**在 KVM 宿主上逐项验证**（这一轮没有在真实宿主上跑过）：

1. `incus --force-local --project cage-tenant config get cage-<id> volatile.vsock_id` 有数字（≥ 3）。
2. 模板的 `cage` 带中继：`echo bad | incus --force-local --project cage-tenant exec cage-<id> -- /usr/local/bin/cage vsock-relay; echo $?` 打印 `cage vsock-relay: invalid configuration on stdin` 并以 2 退出（旧版本打印读不到配置并以 1 退出）。
3. 加 `-incus-vsock` 重启网关，打开一个工作区，日志出现 `dials its workbench through vsock`。宿主上 `ps -eo args | grep -c 'exec .* vsock-relay'` 对每个工作区为 1；`ps -eo args | grep vsock-relay` 里看不到任何十六进制密钥，`sudo cat /proc/<incus exec 的 pid>/environ | tr '\0' '\n' | grep -ci secret` 为 0。
4. 反复打开和关闭终端标签、刷新页面：`ps -eo args | grep -c 'socat STDIO'` 保持不变。默认路径下每个新连接都会让它加 1。
5. 延迟：浏览器 Network 面板里，WebSocket 升级的「等待服务器响应」时间应比默认路径少约一次 `time incus --force-local --project cage-tenant exec cage-<id> -- true` 的耗时。再对比宿主 incusd 的 CPU：浏览器画面流动时应下降。
6. 宿主上的其他进程用不了中继：在宿主上以普通用户执行 `python3 -c 'import socket; s=socket.socket(socket.AF_VSOCK); s.connect((<CID>, <端口>)); print(s.recv(64)); s.send(b"0"*98); print(s.recv(65))'`（端口取日志里那一行），第一行打印问候 `CAGE-VSOCK 1 …`，随后连接被关闭（第二个 `recv` 为空），VM 里 `ss -tnp | grep <工作台端口>` 不出现新的连接。再只连接、不发送：`python3 -c 'import socket,time; s=socket.socket(socket.AF_VSOCK); s.connect((<CID>, <端口>)); s.recv(64); t=time.time(); print(s.recv(1), round(time.time()-t,1))'` 约 3 秒后打印 `b'' 3.0`（中继在 vsock 套接字上的握手超时生效）。
7. VM 里的进程用不了中继：在 VM 内（以及工作区的内层沙箱里，如果它能建 AF_VSOCK 套接字）执行 `python3 -c 'import socket; s=socket.socket(socket.AF_VSOCK); s.connect((1, <端口>)); print(s.recv(64))'` 和把 1 换成 VM 自己的 CID，都应当立刻被关闭（打印 `b''`）或连接失败，得不到问候。另一台租户 VM 连目标 VM 的 CID 和端口必须失败。
8. 中继退出后不会冒充：在 VM 里让中继退出（`pkill -f 'cage vsock-relay'`），网关日志马上出现 `the vsock path ended (the bridge process ended)`；再用 `python3 -c 'import socket; s=socket.socket(socket.AF_VSOCK); s.bind((socket.VMADDR_CID_ANY, <端口>)); s.listen(); s.settimeout(60); c,_=s.accept(); print(c.recv(4096))'` 占住同一端口，在网关侧打开新终端并刷新页面：python 在 60 秒后以 `socket.timeout` 结束（网关不再连这个端口，更不会发任何字节），工作台照常经 exec 可用。关闭再打开该工作区时，网关启动一个新端口、新密钥的中继。
9. 冻结和解冻：让工作区空闲到冻结（第 7 节），再打开工作台；网关恢复实例后新连接仍走 vsock（`ps` 里中继还在），已有的流在冻结时由网关断开、浏览器自动重连，这是已有行为。
10. 网关重启：重启（包括 `kill -9`）后旧中继随标准输入关闭而退出（VM 里 `pgrep -fc 'cage vsock-relay'` 回到每个工作区 1 个），新网关用新的端口和密钥启动自己的中继；VM 关机时中继随之消失。
