# 在手机上使用 Super Cage

Super Cage 手机端是远程客户端。点首页中央的「连接电脑」，用二维码或配对码连接主机，即可查看项目、接续 Claude Code 会话和处理工具审批。配对后还可在手机上操作主机里的浏览器和 Claude Desktop。代理、时区和语言在远程主机的 Super Cage 中设置。右上角的设置提供主题和电脑连接选项。

Android arm64 APK 可从[官网下载入口](https://claude-cage.com/#download)获取，支持 Android 8.0+。iOS 17+ 版本通过 TestFlight 内部测试分发（App「Cage Tenant」，套装 ID `io.github.sweetcornna.claude-cage.tenant.mobile`），也可以用 Xcode 在本地构建并签名安装。

## 示例工作台

首页第三个入口「看看示例工作台」不需要账号、服务器或电脑。点开后进入名为「示例工作台 · 演示数据」的工作台，里面有虚构的项目、会话、工具审批、终端、文件和环境，可以直接试用：发消息会收到预先写好的回复，终端只运行几条读取示例文件的命令，其余操作和远程浏览器、Claude Desktop 一样会提示「示例里不能这样做，连接自己的 Super Cage 后可用」。示例全部在手机内存里运行，不联网、不配对、不保存凭据，也不写入钥匙串或已连接设备列表；点侧栏底部的「退出」回到首页，示例随即丢弃，重启 App 也一样。设置里没有这个入口。

示例由 `mobile/ios/CageMobile/ExampleWorkspace.swift` 实现：手机工作台页面从 App 包内以自己的 URL 协议加载，并在页面开始前注入一段脚本，脚本在内存里回答页面的全部请求。修改后用无头 Chrome 对真实的手机界面核对：

```sh
npm ci --prefix ui && node mobile/build-ui.mjs
CAGE_PLAYWRIGHT_MODULE=<playwright 模块路径> CAGE_TEST_BROWSER_PATH=<Chrome 路径> node mobile/scripts/example-workspace-check.mjs
```

脚本逐屏走查（390×844，触摸），任何未捕获错误、控制台错误、无应答请求、对其他来源的请求都会失败，并在空闲时统计每个界面的请求频率。iPhone 模拟器里另有只在模拟器构建中编译的假扫码入口和 `-preview-screen` 截图参数，真机构建不含它们。

## 连接服务器

首页的「连接服务器」用于多租户 Super Cage 服务器。填写门户地址（只接受 HTTPS 域名）、账户和密码后登录，即可查看、新建和打开自己的虚拟机。停机的虚拟机会先启动，首次可能需要几分钟。打开后工作台列出 Claude Code 项目和会话，可在手机上对话和处理工具审批；远程浏览器与 Claude Desktop 仍需扫码连接电脑。

勾选「记住密码」时，iOS 把密码存进仅本机可用的钥匙串，Android 用系统 Keystore 加密保存。退出登录会关闭这个账户已打开的工作台，虚拟机继续在服务器上运行；在已保存列表中左滑（iOS）或长按（Android）可忘记服务器。

## 附件与导出

工作台网页请求上传文件时，手机打开系统的照片、相机或文件选择器。Android 只有在网页要求直接拍照时才询问相机权限；已允许相机时，文件选择器里也会提供拍照。工作台导出的单个文件不超过 8 MB，通过系统分享面板保存或发送；iOS 第一次把图片存入照片时会请求权限。

## Android 应用更新

打开 Super Cage 或切回前台时，手机每隔至少 12 小时自动检查一次更新，也可以随时在「设置 → 应用更新 → 检查更新」手动检查。正式版只提示更新到更高的正式版，预发行版也会提示更新到更高的预发行版。

发现新版后点「下载更新」。手机校验安装包后会打开系统安装界面。首次更新时，按提示允许 Super Cage 安装来自此来源的应用，返回后点「更新」。覆盖安装会保留主题、设备身份和电脑配对记录。旧版设置里没有「检查更新」时，先从官网下载新版 APK 并覆盖安装一次。

## iOS 本地构建

在 Mac 安装完整 Xcode 与 iOS SDK，并完成 Xcode 首次启动设置。安装 Node.js 24 和符合 `bindings/go.mod` 的 Go 后，在仓库根目录执行。

```sh
npm ci --prefix ui
bash mobile/build.sh ios
open mobile/ios/CageMobile.xcodeproj
```

命令会生成 `mobile/.build/ios/Cagemobile.xcframework` 和未签名的设备构建。涂鸦由 `mobile/assets/brand/doodles` 的 SVG 生成，已提交在 `mobile/ios/CageMobile/Assets.xcassets` 与 `mobile/android/res`；修改后运行 `node mobile/scripts/brand-assets.mjs`，构建脚本会检查这些文件与 SVG 一致。Logo 和应用图标（iOS AppIcon、Android 自适应图标）来自 `brand/` 目录的母版，由 `node brand/generate.mjs` 一并生成，见 `brand/README.md`。在 Xcode 选择 `CageMobile` scheme 和自己的开发团队，连接 iPhone，按系统提示启用开发者模式，再通过 Run 签名安装。需要导出开发 IPA 时运行以下命令，将 `TEAM_ID` 替换为自己的十位 Apple Team ID。

```sh
bash mobile/build.sh ios-ipa TEAM_ID
```

归档与 IPA 保存在 `mobile/.build/ios/device-*/`，导出后会验证设备描述文件与应用签名。

## Android 构建与签名

准备 JDK 21、Android SDK Platform 35、Build Tools 35.0.0 与 NDK 27.2.12479018，并将 `JAVA_HOME`、`ANDROID_HOME` 指向各自的安装目录。安装 UI 依赖后，可在仓库根目录生成本地开发 APK。

```sh
npm ci --prefix ui
bash mobile/build.sh android
```

公开发行使用持久保存的签名密钥和独立的密码文件。以下路径由构建者自行设置，签名材料保存在源码和临时构建目录之外，限制为本人可读。后续 APK 更新沿用同一签名身份，并使用大于上一版的 Android versionCode。[Android 签名说明](https://developer.android.com/studio/publish/app-signing)

```sh
export CAGE_ANDROID_KEYSTORE=/private/signing/release.p12
export CAGE_ANDROID_KEY_ALIAS=release
export CAGE_ANDROID_PASSWORD_FILE=/private/signing/keystore-password
export CAGE_ANDROID_VERSION_CODE=2
bash mobile/build.sh android-release
```

发行 APK 的 versionName 读取根目录 `VERSION`，输出为 `mobile/.build/android/Cage-<版本>-android-arm64.apk`。签名和 16 KiB 对齐检查通过后才写入最终文件。发布时附带 SHA-256，并核对签名证书指纹、应用版本、安装与覆盖更新行为。

## 扫码连接电脑

1. 在电脑上打开并解锁 Super Cage，选择本机工作台，在「设置 → 远程连接 → 手机连接」点「生成配对二维码」。电脑显示二维码后，用手机扫描。
2. 在手机 Super Cage 首页点「连接电脑」，在弹出的卡片里选择扫码，允许使用相机，把电脑上的二维码完整放入取景框中。二维码约两分钟有效，过期后可在电脑上刷新。
3. 扫码成功后手机会保存这台电脑并打开工作台。工作台默认打开左侧栏。点「浏览器」「Claude.ai」或「Claude Desktop」进入对应画面，「Claude Code」分区列出各个项目。点项目名展开或收起会话，点会话进入对话。左上角的 Logo 可重新打开侧栏，右上角图标表示当前工具。下次打开手机，从「连接电脑」卡片重新连接已配对的电脑。电脑重启后，打开 Super Cage 即可恢复扫码配对的连接。

二维码只交给希望授权的手机。要更换扫码配对的手机，先在电脑「设置 → 远程连接」移除旧手机，再为新手机生成二维码。手机可以在指纹配对或主机设置中忘记本机保存的连接。

暂不连接时，可点击卡片外侧收起；Android 也可使用系统返回，iPhone 可使用系统下滑手势。配对过程中需等待本次操作结束。

## 用手机操作远程浏览器

1. 更新电脑和手机的 Super Cage，并在电脑上启动环境，等待网络检查通过。如果界面提示客机版本较旧，在电脑上关闭并重新启动环境，让新的浏览器组件生效。
2. 通过电脑生成的二维码或配对码连接。已配对手机可以直接重新连接。
3. 在手机左侧栏点「浏览器」，会打开电脑 Super Cage 浏览器设置中的「新标签页地址」。电脑保存新地址后，下次从手机打开浏览器即生效。「Claude.ai」入口始终直达 Claude.ai。
4. 点网页中的输入框后，打开「输入文字」使用手机键盘。先点勾号把文字插入网页，需要提交时再点「发送回车」。如果发生断线，先读取最新画面再决定是否重新输入。

点手机浏览器底部的「收藏夹」，可以按电脑上的文件夹浏览收藏、打开网址，或把当前页存入指定文件夹。两端共用配对主机的收藏。另一端有更改时，重新打开收藏夹，或在手机上点刷新即可看到。

手机和电脑各用自己的浏览器窗口，首次访问需要在手机的远程窗口里登录。同一配对重新打开后可以沿用已保存的登录资料。网络出口、时区和首选语言取自主机的 Super Cage。关闭浏览器会结束手机的浏览进程。取消配对或锁定 Super Cage 会中止访问。

手机浏览器按手机宽度请求移动网页。拖动时连续发送滚动位移，页面静止时只检查是否有变化。图像和输入经配对通道传输，反馈速度受网络往返延迟影响。支持公网 IPv4 的 HTTP 和 HTTPS。需要另开弹窗登录、上传或下载附件、播放声音的网页场景尚未接入。模拟器扫码示例用于预览侧栏和对话，测试真实窗口请用配对码连接电脑。

## 用手机操作 Claude Desktop

在电脑的 Super Cage 环境中安装 Claude Desktop 并通过网络检查。已配对的手机连接电脑后打开左侧栏，点「Claude Desktop」，等待应用画面出现。

手机直接访问电脑上同一份 Claude Desktop，共用账号、应用数据和当前窗口。电脑已登录时，手机可以直接接续。手机的点击、输入和切换页面会同步显示在电脑上，两端共用鼠标与输入焦点。应用尚未运行时，由电脑的 Super Cage 启动。

新版手机端会自动读取屏幕像素密度，使远程画面匹配当前显示分辨率。手机进入后，共享窗口按手机宽度重新排版，旋转屏幕或弹出键盘时跟随调整，电脑也会同步显示这个布局。点「放大阅读」可查看局部。退出手机的 Desktop 页面后恢复电脑尺寸，电脑上的应用和任务继续运行。需要重新登录或选择本机文件时，在电脑上完成。

## 使用指纹和文件配对

相机不可用时，在电脑点「复制配对码」，再在手机首页点「连接电脑」，选择配对码入口并粘贴这串一次性配对码。使用命令行连接助手时，从手机右上角的设置进入指纹与连接文件选项，再按以下步骤交换指纹与文件。

在电脑上运行包含移动会话接口的 Super Cage，解锁账户并启动工作环境。电脑上的连接助手可在仓库根目录构建。

```sh
go build -o ./cage-mobile-link ./cmd/cage-mobile-link
```

1. 在电脑上用 `cage-mobile-link init --state <新私有目录>` 创建主机身份，记录显示的公开指纹。以后可用 `fingerprint --state <原目录>` 再次查看。
2. 手机打开 Super Cage，在“这台手机的指纹”下复制公开指纹。对照手机屏幕，在电脑上执行 `cage-mobile-link trust --state <原目录> --peer-pin <已核对的手机指纹>`。
3. 在电脑上生成这台手机的连接文件，并保持连接助手运行。官方中继允许直接创建连接。请填写主机身份目录、新连接文件路径，以及 Super Cage 后台程序和配置文件的绝对路径。

```sh
./cage-mobile-link host --state "$host_state" --connection-out "$new_connection_json" \
  --cage-bin "$cage_binary" --config "$cage_config"
```

4. 通过自己的私有文件传输方式将 JSON 送到手机。在 App 中点“导入连接文件”，对照**电脑独立显示的公开指纹**，确认与手机上填入的指纹一致。勾选核对项，点“保存已核对的主机”，然后点“重新连接”。换用另一台电脑或更换电脑密钥时需再次核对。

进入工作台后，在左侧栏的「Claude Code」下选择项目，新建或继续会话。工具需要授权时，手机显示本次调用的具体参数，可选择“允许一次”或“拒绝”。此入口列出通过 Super Cage 会话界面创建的对话，普通终端里手工启动的 CLI 仍在原终端操作。

续聊前，Super Cage 检查原会话使用的账号和模型服务地址，避免把历史发到另一个服务。主机无法确认所用配置的身份时，手机会提示本次仅支持一轮；可以新建会话再发消息。

## 使用自己的 VPS

部署[自建中继](../deploy/relay/README.md)后，在桌面「设置 → 远程连接」选择「自建中继」，填写 VPS 的 HTTPS 地址并保存。手机进入设置并展开「中继设置」，开启自建中继，填写同一地址并保存。需要管理令牌的自建中继只在电脑端填写令牌。选择官方中继并保存可恢复默认。新配对采用保存的地址，手机导入连接文件时会自动填入同一地址。自建中继要求管理令牌时，在上面的 host 命令中同时指定 `--relay` 和 `--admin-token-file`，明确私有令牌的接收方。

已配对的手机继续使用原来的地址与凭据。要迁往另一台中继，扫码设备在桌面设置中移除后重新扫码；命令行配对设备用下文的 revoke 撤销后重新登记。

## 下次使用

点左上角的 Logo 打开侧栏，点侧栏底部「退出」回到首页。需要切换电脑时，点首页的「连接电脑」，在卡片的「已连接设备」中选另一台电脑。长按设备可取消这次连接，配对记录会保留。

Claude Code 的项目可以各自展开，方便在不同项目的会话之间切换。会话进度自动刷新；读取失败时可以点提示中的「重新读取」。聊天通过气泡区分消息来源，输入框为大圆角，右侧箭头用于发送，运行时显示停止按钮。

在已配对的手机上，短暂断网时可以留在当前会话。网络恢复后，工作台再次读取进度时会重新验证电脑身份并恢复连接，输入框里的草稿仍在。若发消息或审批时断线，先看电脑返回的最新记录。已收到的消息会显示在历史里；电脑仍在等待同一项审批时，手机会重新显示可选的「允许一次」和「拒绝」。

主动断开或重新打开 App 后，从「连接电脑」卡片选择已配对的电脑，再打开原会话即可继续。如果使用命令行连接助手，电脑重启后先启动 Super Cage，再运行以下命令恢复该助手的配对：

```sh
./cage-mobile-link host --state "$host_state" --serve-saved
```

连接文件包含中继地址、电脑公开指纹和持久查询凭据，请只交给配对手机。Android 用独立的 Android Keystore AES-GCM 密钥加密查询凭据，并将密文绑定到已核对的中继地址和指纹。iPhone 将记录保存在仅限本设备、解锁时可访问的 Keychain 项中。设备签名私钥由 Android Keystore 或 iPhone Secure Enclave 保管。仅 iPhone 模拟器的 Debug 构建使用单独的软件测试密钥，设置中的设备指纹旁会注明。网页只访问本机受限的会话接口。

在手机点“忘记主机”会清除保存的主机地址、指纹和重连凭据，手机仍使用自己的设备签名身份。要同时在电脑端撤销访问，执行 `cage-mobile-link revoke --state <主机身份目录>`。使用旧版单次邀请时，在电脑以 `--invitation-out <新邀请文件>` 代替 `--connection-out`，手机手工核对并保存中继地址与电脑指纹，再通过同一个导入入口打开邀请。单次邀请断开后需要新邀请。
