---
title: Claude Science
description: 在 x86-64 Linux 环境中按需安装 Claude Science，在 Super Cage 浏览器内登录和使用，在 Windows 窗口中试用本机渲染，了解磁盘需求与当前限制。
group: 日常工作
order: 6.2
keywords: Claude Science Operon 科研 数据分析 Python R 安装 登录 磁盘 arm64 本机渲染
---
## 哪些环境可以使用

Claude Science 是 Anthropic 的科研与数据分析应用。在 Super Cage 中，它的后台服务运行在环境内，页面在 Super Cage 浏览器的一个标签页中打开。

目前只支持 **x86-64 的 Linux 环境**，例如 Intel 或 AMD 电脑上的 Debian 环境，或通过[云端部署与 SSH](cloud.md)连接的 x86-64 Linux 服务器。以下情况暂不能安装，工作台会说明原因：

- Apple 芯片 Mac 与 ARM 服务器上的环境：官方尚未提供 Linux arm64 版本。
- macOS 与 Windows 客机：本阶段只接入 Linux 版本。
- 系统自带 bubblewrap 低于 0.8 的客机，例如 Ubuntu 22.04：Claude Science 无法启动它的沙箱。Debian 13 符合要求。
- 用户目录位于临时文件系统的环境：Claude Science 拒绝把项目和会话保存在重启后会清空的位置。

## 磁盘与版本

首次安装需要客机至少 11 GB 可用空间：程序本身约 170 MB，首次启动还会准备约 10 GB 的 Python 与 R 环境，可能需要几分钟。之后更新只需约 1 GB。空间不足时安装会在下载前停止，并显示当前可用空间。Debian 环境的虚拟磁盘上限为 24 GiB，请先在终端中删除不再需要的项目产物或下载文件后重试。

Super Cage 为每个版本固定一个经过核对的 Claude Science 版本，下载后先校验 SHA-256，校验不符时不会安装任何文件。应用内的自动更新已关闭，新版本随 Super Cage 更新提供。若 Claude Science 提示必须升级：当前 Super Cage 提供的版本已满足要求时，点击“更新”即可；仍不满足时，请先更新 Super Cage。

## 安装、启动与登录

1. 启动环境，确认网络检查通过。
2. 在 Claude Science 入口选择安装。下载在环境内经所选代理进行，进度可在操作详情查看。
3. 安装完成后启动 Claude Science，再打开它。Super Cage 在浏览器窗格中打开一个名为“Claude Science”的标签页。
4. 首次打开时，页面先要求确认一次，然后显示 Claude Science 的登录页。点击“Sign in with Claude”，在同一个标签页中登录 Claude 账号并授权。授权完成后页面自动回到 Claude Science。

登录完全发生在环境内的浏览器中，不导入主机上的账号，主机上的程序也不会代为联系 Anthropic。登录状态保存在环境内，重启环境或 Claude Science 后仍然有效。

每次打开都会生成一个只能使用一次、几分钟内有效的链接，不要复制给他人。标签页显示链接失效时，回到入口重新打开即可。已打开的 Claude Science 标签页会被直接切换到前台，不会重复打开。

## 网络、隐私与文件

Claude Science 的服务只在环境内的本机地址监听，端口不会转发到主机或局域网，外部设备无法访问。开启下文的本机渲染后，主机上的 Super Cage 经与环境的加密连接转交页面请求，只供这台电脑上的 Super Cage 窗口使用，同样不对局域网开放。它的网络请求和环境中其他程序一样，经过 Super Cage 的代理出口。Super Cage 按官方文档关闭了它的使用数据上报。

复制粘贴、下载与上传遵循[浏览器与网站数据](browser.md)中的规则。Claude Science 的项目、会话和环境保存在客机用户目录的 `.claude-science` 中，随环境保留。

## 本机渲染（实验性）

只在 Windows 版 Super Cage 窗口中提供，默认关闭，每个环境分别设置。开启后，Claude Science 的页面直接显示在 Super Cage 窗口里，不再经过浏览器窗格的画面传输，滚动和输入没有额外延迟。页面仍由环境内的 Claude Science 服务提供，看到的时区、语言和浏览器标识是环境的，不是这台电脑的。

在 Claude Science 页面卡片下方打开“本机渲染（实验性）”。第一次开启时，按提示重新载入窗口一次，让窗口带上本机渲染需要的网络限制；之后进入 Claude Science 页面即直接显示。关闭开关会立即回到浏览器窗格方式，窗口的网络限制在下次重新载入窗口后解除。

开启后：

- 每次打开时，窗口先核对页面看到的时区、语言、浏览器标识和网络限制。任何一项不符，这次改用隔离浏览器打开，并说明原因；可以在页面上重试。
- 页面的请求只交给主机上的 Super Cage，再经与环境的加密连接转交 Claude Science。页面不能直接连接其他网站；请求中出现这台电脑的时区或语言时会被拦截，工具栏会提示。
- 登录 Claude 账号仍在隔离浏览器中完成：点击页面中的登录，或工具栏中的“登录”，Super Cage 会切换到浏览器窗格中的登录页。
- 停留在 Claude Science 页面期间，整个工作台窗口都使用环境的时区与语言；离开该页面后恢复。
- 离开页面、锁定 Super Cage、停止环境或 Claude Science、切换虚拟机后，主机上的 Super Cage 会结束这次会话并丢弃从环境取得的凭据；切换虚拟机或关闭窗口时，最迟约 90 秒后结束。
- 窗口本身不再连接外部网络（工作台始终只连接这台电脑上的 Super Cage），环境与 Super Cage 后台的联网不受影响。

当前限制：

- macOS 与 Linux 窗口、浏览器、手机和远程主机继续使用浏览器窗格。
- 不支持下载、文件选择、连接器授权和预览沙箱；需要时点击工具栏中的“在隔离浏览器中打开”。
- 页面可以把内容复制到剪贴板；页面主动读取剪贴板的请求会被拒绝，在输入框中粘贴不受影响。
- 每次进入 Claude Science 页面都会重新加载页面。
- Super Cage 刚启动时窗口可能会自动重新载入一次，之后再次进入 Claude Science 页面即可。

## 当前阶段尚不提供

- macOS 与 Linux 窗口中的本机渲染。这些窗口通过浏览器窗格使用 Claude Science，操作延迟与浏览器窗格相同。
- macOS 客机、Windows 客机与 arm64 环境。
- 跟随应用内更新。版本由 Super Cage 决定，请通过 Super Cage 更新。
- 在主机的浏览器中直接访问 Claude Science 的网页或端口。本机渲染只供 Super Cage 窗口使用。
