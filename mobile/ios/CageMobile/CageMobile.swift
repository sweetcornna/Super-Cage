import Foundation
import SwiftUI
import VisionKit
import AVFoundation
import UniformTypeIdentifiers
import WebKit
private var simulatorPreviewEnabled: Bool {
    #if targetEnvironment(simulator)
    return true
    #else
    return false
    #endif
}

struct ConnectedDevice: Identifiable {
    let id: String
    var name: String
    let host: Host?
    let connection: CMConnection?
    let webView: WKWebView
    /// Set for a virtual machine opened from a Cage server account.
    var account: ServerAccount? = nil
    /// Set for the example workspace: no connection, answered from memory by ExampleWorkspace.fixture.
    var isExample = false
}

final class DeviceWebBridge: NSObject, WKScriptMessageHandler {
    weak var model: ConnectionModel?
    let id: String
    let origin: URL
    init(model: ConnectionModel, id: String, origin: URL) {self.model = model; self.id = id; self.origin = origin}
    func userContentController(_ userContentController: WKUserContentController, didReceive message: WKScriptMessage) {
        guard let model, model.devices.contains(where: {$0.id == id && $0.webView === message.webView}),
              message.frameInfo.isMainFrame, let url = message.frameInfo.request.url,
              url.scheme == origin.scheme, url.host == origin.host, url.port == origin.port,
              let body = message.body as? [String: Any], let action = body["action"] as? String else {return}
        if action == "name" {
            if let name = body["name"] as? String {model.nameDevice(id, name)}
            return
        }
        guard model.showingWorkspace, model.selectedDeviceID == id else {return}
        if action == "home" {model.leaveWorkspace()}
        // The example never opens other sites.
        if action == "open-url", model.devices.first(where: {$0.id == id})?.isExample != true, let raw = body["url"] as? String, raw.utf8.count <= 16384, let url = URL(string: raw),
           ["http", "https"].contains(url.scheme?.lowercased() ?? ""), url.host != nil, url.user == nil {
            UIApplication.shared.open(url)
        }
        if action == "haptic", let style = body["style"] as? String {WorkbenchActions.haptic(style)}
        if action == "share-file", let web = message.webView, let name = body["name"] as? String,
           let data = body["data"] as? String {
            WorkbenchActions.share(name: name, mime: body["mime"] as? String ?? "", data: data, from: web)
        }
    }
}

final class ConnectionModel: ObservableObject {

    @Published var relay = CMConnection.defaultRelay()
    @Published var pin = ""
    @Published var fingerprint = ""
    @Published var verified = false
    @Published var hasInvitation = false
    @Published var hasSaved = false
    @Published var needsTrust = false
    @Published var status = ""
    @Published var connecting = false
    @Published var session: CMConnection?
    @Published var devices: [ConnectedDevice] = []
    @Published var selectedDeviceID: String?
    @Published var showingWorkspace = false
    var selectedDevice: ConnectedDevice? {devices.first {$0.id == selectedDeviceID}}

    private var invitation: Invitation?
    private var imported: HostAccess?
    private var trusted: Host?
    private var generation = 0
    private var openArchivedAfterConnect = false
    private let trustKey = "independently-verified-host-v1"
    private let linkQueue = DispatchQueue(label: "CageMobile.connection")
    var canForget: Bool { trusted != nil }
    var canConnect: Bool { !connecting && !needsTrust && !fingerprint.isEmpty && (hasInvitation || hasSaved) }

    init() {
        if let data = UserDefaults.standard.data(forKey: trustKey), let host = try? JSONDecoder().decode(Host.self, from: data) {
            trusted = host; relay = host.relay; pin = host.pin
        }
        do {
            if let saved = try HostKeychain.read() {
                trusted = saved.host; relay = saved.host.relay; pin = saved.host.pin; hasSaved = true
            }
        } catch {
            status = "无法读取已保存的连接，请解锁手机后重新打开。"
        }
        if let selected = UserDefaults.standard.string(forKey:"preferred-relay") {relay = selected}
        DispatchQueue.global(qos: .userInitiated).async {
            do {
                let value = try CMConnection.deviceFingerprint()
                DispatchQueue.main.async { self.fingerprint = value }
            } catch {
                DispatchQueue.main.async { self.status = error.localizedDescription }
            }
        }
    }
    func pair(code: String) {
        guard !connecting else {return}
        connecting = true; generation += 1
        let attempt = generation
        let address = UserDefaults.standard.string(forKey: "preferred-relay") ?? trusted?.relay ?? CMConnection.defaultRelay()
        status = "正在与电脑配对…"
        linkQueue.async {
            let result = Result { () throws -> HostAccess in
                let data = Data(try CMConnection.pair(code: code, relay: address).utf8)
                guard case .persistent(let access) = try ConnectionFile.parse(data) else {throw InputError.invalid}
                return access
            }
            DispatchQueue.main.async {
                guard attempt == self.generation else {return}
                self.connecting = false
                do {
                    let saved = try result.get()
                    try HostKeychain.write(saved)
                    UserDefaults.standard.set(saved.host.relay,forKey:"preferred-relay")
                    self.trusted = saved.host; self.pin = saved.host.pin; self.relay = saved.host.relay
                    self.imported = nil; self.invitation = nil; self.hasInvitation = false
                    self.hasSaved = true; self.needsTrust = false; self.verified = false
                    let hostID = saved.host.relay + "|" + saved.host.pin
                    if self.devices.contains(where: {$0.id == hostID}) {self.disconnectDevice(hostID)}
                    self.connect()
                } catch { self.status = error.localizedDescription }
            }
        }
    }
    func saveRelay() {
        let input = relay.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let u = URLComponents(string: input), u.scheme?.lowercased() == "https",
              let host=u.host, !host.isEmpty, u.user==nil, u.password==nil, u.query==nil, u.fragment==nil,
              (u.path=="" || u.path=="/"), (1...65535).contains(u.port ?? 443) else {status="请填写有效的 HTTPS 地址。";return}
        relay=input.hasSuffix("/") ? String(input.dropLast()) : input
        UserDefaults.standard.set(relay,forKey:"preferred-relay")
        status="中继设置已保存。"
    }
    func saveTrust() {
        guard verified, let host = try? Host(relay: relay, pin: pin), let data = try? JSONEncoder().encode(host) else {
            status = "请填写有效的中继地址，核对电脑指纹后再保存。"; return
        }
        do {
            if let record = imported {
                guard record.host == host else { status = "填写内容与连接文件不一致，请重新核对。"; return }
                try HostKeychain.write(record)
                hasSaved = true
            } else if trusted != host {
                hasSaved = try HostKeychain.read(host: host) != nil
            }
        } catch { status = "无法安全保存主机，请解锁手机后重试。"; return }
        trusted = host; UserDefaults.standard.set(host.relay,forKey:"preferred-relay"); UserDefaults.standard.set(data, forKey: trustKey); verified = false
        imported = nil; needsTrust = false
        status = hasSaved ? "已保存这台电脑，可以重新连接。" : "已保存主机指纹。"
    }
    func loadInvitation(_ file: URL) {
        invitation = nil; imported = nil; needsTrust = false; hasInvitation = false; verified = false
        do {
            switch try ConnectionFile.load(file) {
            case .temporary(let room):
                invitation = room; hasInvitation = true; status = "已导入本次临时邀请。"
            case .persistent(let saved):
                imported = saved; needsTrust = true; relay = saved.host.relay; pin = saved.host.pin
                status = "请与电脑上独立显示的指纹核对，再保存这台主机。"
            }
        } catch { status = "文件无效或临时邀请已过期，请重新导入。" }
    }
    func forget() {
        do { try HostKeychain.forget(host: trusted) } catch { status = "移除未完成，请解锁手机后重试。"; return }
        UserDefaults.standard.removeObject(forKey: trustKey)
        trusted = nil; imported = nil; invitation = nil; relay = UserDefaults.standard.string(forKey:"preferred-relay") ?? CMConnection.defaultRelay(); pin = ""
        hasSaved = false; needsTrust = false; verified = false; hasInvitation = false
        status = "已忘记主机。"
    }
    func connect() {
        connect(openArchived: false)
    }
    private func connect(openArchived: Bool) {
        guard !connecting, !needsTrust, let host = trusted else {
            status = "请先核对并保存这台主机。"; return
        }
        openArchivedAfterConnect = openArchived
        if let existing = devices.first(where: {$0.id == host.relay + "|" + host.pin}) {
            selectDevice(existing.id)
            if openArchivedAfterConnect {
                openArchivedAfterConnect = false
                existing.webView.evaluateJavaScript("window.dispatchEvent(new Event('cage-open-archives'))")
            }
            return
        }
        let item = invitation

        guard item != nil || hasSaved else { status = "请导入连接文件或有效的临时邀请。"; return }
        if let item, (item.expiry ?? .distantPast) <= Date() { invitation = nil; hasInvitation = false; status = "临时邀请已过期，请重新连接或导入新邀请。"; return }
        guard let directory = Bundle.main.url(forResource: "mobile-ui", withExtension: nil) else { status = "安装包缺少工作台资源。"; return }
        invitation = nil; hasInvitation = false; connecting = true; generation += 1
        let attempt = generation
        status = "正在验证电脑并连接…"
        linkQueue.async {
            let result = Result {
                if let item { return try CMConnection.connect(relay: host.relay, room: item.id, token: item.client_token, pin: host.pin, assets: directory.path) }
                guard let saved = try HostKeychain.read(host: host), saved.host == host else { throw InputError.unavailable }
                return try CMConnection.connectSaved(relay: host.relay, lookup: saved.lookup, pin: host.pin, assets: directory.path)
            }
            DispatchQueue.main.async {
                guard attempt == self.generation else {
                    if case .success(let old) = result { DispatchQueue.global().async { old.close() } }
                    return
                }
                self.connecting = false
                switch result {
                case .success(let session):
                    self.status = ""
                    if self.didBackground {
                        self.linkQueue.async { session.suspend() }
                    }
                    self.addDevice(session, host: host)
                case .failure(let error): self.status = error.localizedDescription
                }
            }
        }
    }
    func leaveWorkspace() {
        showingWorkspace = false
        // The example holds nothing worth resuming: leaving it discards it, and the next visit starts from the sample data again.
        if selectedDeviceID == ExampleWorkspace.deviceID {
            DispatchQueue.main.async {self.disconnectDevice(ExampleWorkspace.deviceID)}
        }
    }
    /// Opens the example workspace from the start page. Everything stays in memory.
    func openExample() {
        if !devices.contains(where: {$0.id == ExampleWorkspace.deviceID}) {
            addExample(id: ExampleWorkspace.deviceID, name: ExampleWorkspace.deviceName)
        }
        selectDevice(ExampleWorkspace.deviceID)
    }
    private func addExample(id: String, name: String) {
        let example = ExampleWorkspace.makeWebView(model: self, id: id)
        devices.append(ConnectedDevice(id: id, name: name, host: nil, connection: nil, webView: example.web, isExample: true))
        navigationDelegates[id] = example.delegate
    }
    func openArchivedSessions() {
        if let device = selectedDevice {
            selectDevice(device.id)
            device.webView.evaluateJavaScript("window.dispatchEvent(new Event('cage-open-archives'))")
        } else {
            connect(openArchived: true)
        }
    }
    func selectDevice(_ id: String) {
        guard devices.contains(where: {$0.id == id}) else {return}
        selectedDeviceID = id; showingWorkspace = true
        session = selectedDevice?.connection
        publishDevices()
    }
    func nameDevice(_ id: String, _ value: String) {
        guard let index = devices.firstIndex(where: {$0.id == id}), devices[index].account == nil else {return}
        let name = value.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !name.isEmpty, devices[index].name != name else {return}
        devices[index].name = String(name.prefix(120)); publishDevices()
    }
    private func addDevice(_ connection: CMConnection, host: Host) {
        addDevice(connection, id: host.relay + "|" + host.pin, name: "已连接电脑", host: host, account: nil)
    }
    func addServerDevice(_ connection: CMConnection, id: String, name: String, account: ServerAccount) {
        if devices.contains(where: {$0.id == id}) {disconnectDevice(id)}
        if didBackground {linkQueue.async {connection.suspend()}}
        addDevice(connection, id: id, name: name, host: nil, account: account)
    }
    func disconnectServer(_ account: ServerAccount) {
        for device in devices where device.account == account {disconnectDevice(device.id)}
    }
    private func addDevice(_ connection: CMConnection, id: String, name: String, host: Host?, account: ServerAccount?) {
        let config = WKWebViewConfiguration()
        config.websiteDataStore = .nonPersistent()
        config.allowsInlineMediaPlayback = true
        config.preferences.javaScriptCanOpenWindowsAutomatically = false
        config.applicationNameForUserAgent = "CageMobile/1"
        let delegate = WorkspaceNavigation(connection.launchURL)
        config.userContentController.add(DeviceWebBridge(model: self, id: id, origin: connection.launchURL), name: "cageDevices")
        config.userContentController.addUserScript(WKUserScript(source: deviceScript(id), injectionTime: .atDocumentStart, forMainFrameOnly: true))
        let web = WKWebView(frame: .zero, configuration: config)
        web.scrollView.contentInsetAdjustmentBehavior = .never
        web.usePaperBackground()
        web.navigationDelegate = delegate
        // WKWebView holds its navigation delegate weakly.
        navigationDelegates[id] = delegate
        devices.append(ConnectedDevice(id: id, name: name, host: host, connection: connection, webView: web, account: account))
        selectDevice(id)
        var launch = connection.launchURL
        if openArchivedAfterConnect {
            var parts = URLComponents(url: launch, resolvingAgainstBaseURL: false)!
            parts.queryItems = (parts.queryItems ?? []) + [URLQueryItem(name: "page", value: "archived")]
            launch = parts.url!
            openArchivedAfterConnect = false
        }
        web.load(URLRequest(url: launch))
    }
    private var navigationDelegates: [String: WKNavigationDelegate] = [:]
    private func deviceScript(_ current: String) -> String {
        let value: [String: Any] = ["current": current, "devices": devices.map {
            ["id": $0.id, "name": $0.name, "kind": $0.account == nil ? "computer" : "tenant"]
        }]
        let data = try! JSONSerialization.data(withJSONObject: value, options: [.sortedKeys])
        return "window.__cageDevices=" + String(decoding: data, as: UTF8.self) + ";window.dispatchEvent(new CustomEvent('cage-devices',{detail:window.__cageDevices}));"
    }
    func publishDevices() {
        for device in devices {
            let script = deviceScript(device.id)
            device.webView.configuration.userContentController.removeAllUserScripts()
            device.webView.configuration.userContentController.addUserScript(WKUserScript(source: script, injectionTime: .atDocumentStart, forMainFrameOnly: true))
            // The example's script is installed separately and must survive the removal above, on every build:
            // without it the page would ask the network for its data.
            if device.isExample {
                device.webView.configuration.userContentController.addUserScript(WKUserScript(source: ExampleWorkspace.fixture, injectionTime: .atDocumentStart, forMainFrameOnly: true))
            }
            device.webView.evaluateJavaScript(script)
        }
    }
    func disconnectDevice(_ id: String) {
        guard let old = devices.first(where: {$0.id == id}) else {return}
        old.webView.stopLoading(); old.webView.navigationDelegate = nil
        old.webView.configuration.userContentController.removeScriptMessageHandler(forName: "cageDevices")
        devices.removeAll {$0.id == id}; navigationDelegates.removeValue(forKey: id)
        if selectedDeviceID == id {
            selectedDeviceID = devices.first?.id; session = selectedDevice?.connection
            if selectedDeviceID == nil {showingWorkspace = false}
        }
        if let connection = old.connection {linkQueue.async {connection.close()}}
        publishDevices()
    }
    func disconnect() {if let id = selectedDeviceID {disconnectDevice(id)}}
    func reconnect() {disconnect(); connect()}
    func reconnectSaved() {
        if let host = trusted {
            let id = host.relay + "|" + host.pin
            if devices.contains(where: {$0.id == id}) {disconnectDevice(id)}
        }
        connect()
    }
    private var didBackground = false
    private var lifecycleGeneration = 0
    private func connectSavedHost(_ host: Host) {
        guard !connecting,
              let directory = Bundle.main.url(forResource: "mobile-ui", withExtension: nil) else {
            status = "设备连接已失效，请重新连接。"; return
        }
        connecting = true; generation += 1
        let attempt = generation
        status = "正在恢复与电脑的连接…"
        linkQueue.async {
            let result = Result {
                guard let saved = try HostKeychain.read(host: host), saved.host == host else { throw InputError.unavailable }
                return try CMConnection.connectSaved(relay: host.relay, lookup: saved.lookup, pin: host.pin, assets: directory.path)
            }
            DispatchQueue.main.async {
                guard attempt == self.generation else {
                    if case .success(let old) = result { DispatchQueue.global().async { old.close() } }
                    return
                }
                self.connecting = false
                switch result {
                case .success(let session):
                    self.status = ""
                    if self.didBackground {
                        self.linkQueue.async { session.suspend() }
                    }
                    self.addDevice(session, host: host)
                case .failure(let error): self.status = error.localizedDescription
                }
            }
        }
    }
    func handleBackground() {
        didBackground = true
        lifecycleGeneration += 1
        let connections = devices.compactMap(\.connection)
        guard !connections.isEmpty else {return}
        var taskID = UIBackgroundTaskIdentifier.invalid
        taskID = UIApplication.shared.beginBackgroundTask(withName: "CageMobile.suspend") {
            if taskID != .invalid {
                UIApplication.shared.endBackgroundTask(taskID)
                taskID = .invalid
            }
        }
        linkQueue.async {
            for connection in connections {connection.suspend()}
            DispatchQueue.main.async {
                if taskID != .invalid {
                    UIApplication.shared.endBackgroundTask(taskID)
                    taskID = .invalid
                }
            }
        }
    }
    func handleForeground() {
        guard didBackground else {return}
        didBackground = false
        lifecycleGeneration += 1
        let lifecycle = lifecycleGeneration
        let snapshot = devices
        guard !snapshot.isEmpty else {return}
        linkQueue.async {
            for device in snapshot {
                guard let connection = device.connection else {continue}
                let ok = connection.resume()
                DispatchQueue.main.async {
                    guard self.lifecycleGeneration == lifecycle, !self.didBackground else {return}
                    guard let current = self.devices.first(where: {$0.id == device.id}),
                          current.connection === connection else {return}
                    if ok {
                        current.webView.evaluateJavaScript("window.dispatchEvent(new Event('online'))")
                    } else {
                        let wasSelected = (self.selectedDeviceID == device.id && self.showingWorkspace)
                        self.disconnectDevice(device.id)
                        if wasSelected, let host = device.host, (try? HostKeychain.read(host: host)) != nil {
                            self.connectSavedHost(host)
                        } else {
                            self.status = "部分设备的连接已失效，请重新连接。"
                        }
                    }
                }
            }
        }
    }
    /// Simulator only (fake QR scan, -preview-screen): two example devices for screenshots of the device list.
    func startPreview() {
        #if targetEnvironment(simulator)
        for (id, name) in [("preview-mac", "我的电脑 · 模拟器预览"), ("preview-linux", "Linux 工作站 · 模拟器预览")] {
            if !devices.contains(where: {$0.id == id}) {addExample(id: id, name: name)}
        }
        selectDevice("preview-mac")
        #endif
    }
}

private final class WorkspaceNavigation: NSObject, WKNavigationDelegate {
    let origin: URL
    init(_ origin: URL) {self.origin = origin}
    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction,
                 decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        guard let url = action.request.url, action.targetFrame?.isMainFrame == true,
              url.scheme == origin.scheme, url.host == origin.host, url.port == origin.port,
              url.user == nil, url.password == nil else {decisionHandler(.cancel); return}
        decisionHandler(.allow)
    }
}

private struct Workspace: UIViewRepresentable {
    let webView: WKWebView
    func makeUIView(context: Context) -> WKWebView {webView}
    func updateUIView(_ view: WKWebView, context: Context) {}
}

/// Super Cage palette: ivory paper, dark ink, one clay accent (light / dark).
enum CageStyle {
    private static func dynamic(_ light: UInt32, _ dark: UInt32) -> UIColor {
        func color(_ hex: UInt32) -> UIColor {
            UIColor(red: CGFloat(hex >> 16 & 0xff) / 255, green: CGFloat(hex >> 8 & 0xff) / 255, blue: CGFloat(hex & 0xff) / 255, alpha: 1)
        }
        let (day, night) = (color(light), color(dark))
        return UIColor { $0.userInterfaceStyle == .dark ? night : day }
    }
    static let paperColor = dynamic(0xFAF9F5, 0x1F1E1D)
    static let paper = Color(uiColor: paperColor)
    static let surface = Color(uiColor: dynamic(0xFFFFFF, 0x2A2926))
    static let surface2 = Color(uiColor: dynamic(0xF0EEE6, 0x34322E))
    static let line = Color(uiColor: dynamic(0xE6E3D8, 0x3D3A35))
    static let ink = Color(uiColor: dynamic(0x1F1E1D, 0xF3EFE5))
    static let ink2 = Color(uiColor: dynamic(0x6B655B, 0xABA597))
    static let clay = Color(red: 0xD9 / 255, green: 0x77 / 255, blue: 0x57 / 255)
    static let clayInk = Color(uiColor: dynamic(0xA4482A, 0xE8936F))
    static let danger = Color(uiColor: dynamic(0xB3261E, 0xF2B8B5))
    static let ok = Color(uiColor: dynamic(0x2E7D4F, 0x7BC79A))
    static func serif(_ size: CGFloat, _ weight: Font.Weight = .regular) -> Font {
        .system(size: size, weight: weight, design: .serif)
    }
}

extension WKWebView {
    /// Matches the workbench page background so loading never flashes white.
    func usePaperBackground() {
        isOpaque = false
        backgroundColor = CageStyle.paperColor
        scrollView.backgroundColor = CageStyle.paperColor
    }
}

extension View {
    /// A text field on a soft rounded well.
    func cageField() -> some View {
        padding(.horizontal, 14).frame(minHeight: 52)
            .background(CageStyle.surface2, in: RoundedRectangle(cornerRadius: 12))
            .foregroundStyle(CageStyle.ink)
    }
    /// Paper background with white cards for Form and List screens.
    func cageForm() -> some View {
        scrollContentBackground(.hidden).background(CageStyle.paper)
    }
}

struct CageButtonStyle: ButtonStyle {
    @Environment(\.isEnabled) private var enabled
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .opacity(enabled ? (configuration.isPressed ? 0.82 : 1) : 0.45)
    }
}

/// Primary actions are ink pills with paper text; secondary ones sit on the card with a hairline.
struct ActionLabel: View {
    let title: String
    var symbol: String? = nil
    var primary = true
    var danger = false
    var height: CGFloat = 52
    var body: some View {
        HStack(spacing: 8) {
            if let symbol {Image(systemName: symbol).font(.body.weight(.medium)).accessibilityHidden(true)}
            Text(title)
        }
        .font(.system(size: 17, weight: .medium)).frame(maxWidth: .infinity, minHeight: height)
        .foregroundStyle(danger ? CageStyle.danger : primary ? CageStyle.paper : CageStyle.ink)
        .background(danger ? CageStyle.surface2 : primary ? CageStyle.ink : CageStyle.surface, in: RoundedRectangle(cornerRadius: 14))
        .overlay {if !primary && !danger {RoundedRectangle(cornerRadius: 14).strokeBorder(CageStyle.line)}}
        .contentShape(Rectangle())
    }
}

/// A small pill for a state such as 运行中.
struct StateChip: View {
    let text: String
    let tone: Color
    var body: some View {
        Text(text).font(.caption.weight(.medium)).foregroundStyle(tone)
            .padding(.horizontal, 10).padding(.vertical, 4)
            .background(tone.opacity(0.14), in: Capsule())
    }
}

/// Light haptics and file export requested by the phone workbench through the
/// cageDevices bridge: {action: "haptic", style} and {action: "share-file", name, mime, data}.
enum WorkbenchActions {
    private static var sharing = false
    static func haptic(_ style: String) {
        switch style {
        case "light": UIImpactFeedbackGenerator(style: .light).impactOccurred()
        case "medium": UIImpactFeedbackGenerator(style: .medium).impactOccurred()
        case "success": UINotificationFeedbackGenerator().notificationOccurred(.success)
        case "warning": UINotificationFeedbackGenerator().notificationOccurred(.warning)
        default: break
        }
    }
    static func share(name: String, mime: String, data: String, from web: WKWebView) {
        guard !sharing else {return}
        sharing = true
        DispatchQueue.global(qos: .userInitiated).async {
            let result = Result { try CMSharedFile.write(name: name, mime: mime, data: data) }
            DispatchQueue.main.async {
                sharing = false
                var top = web.window?.rootViewController
                while let next = top?.presentedViewController {top = next}
                guard let presenter = top, !presenter.isBeingDismissed, !(presenter is UIActivityViewController) else {
                    if case .success(let file) = result {try? FileManager.default.removeItem(at: file.deletingLastPathComponent())}
                    return
                }
                switch result {
                case .success(let file):
                    let sheet = UIActivityViewController(activityItems: [file], applicationActivities: nil)
                    sheet.completionWithItemsHandler = {_, _, _, _ in try? FileManager.default.removeItem(at: file.deletingLastPathComponent())}
                    sheet.popoverPresentationController?.sourceView = web
                    sheet.popoverPresentationController?.sourceRect = CGRect(x: web.bounds.midX, y: web.bounds.maxY - 1, width: 1, height: 1)
                    presenter.present(sheet, animated: true)
                case .failure(let error):
                    let alert = UIAlertController(title: "无法分享", message: error.localizedDescription, preferredStyle: .alert)
                    alert.addAction(UIAlertAction(title: "好", style: .cancel))
                    presenter.present(alert, animated: true)
                }
            }
        }
    }
}

private struct QRScanner: UIViewControllerRepresentable {
    @Binding var hint: String
    let result: (String?) -> Void
    final class Delegate: NSObject, DataScannerViewControllerDelegate {
        let result: (String?) -> Void
        var hint: Binding<String>
        var finished = false
        init(_ result: @escaping (String?) -> Void, hint: Binding<String>) {self.result = result; self.hint = hint}
        private func handle(_ items: [RecognizedItem], in scanner: DataScannerViewController) {
            guard !finished else {return}
            var sawInvalidQR = false
            for case .barcode(let barcode) in items {
                guard let code = barcode.payloadStringValue else {continue}
                if code.hasPrefix("CAGE1:"), code.utf8.count <= 4096 {
                    UINotificationFeedbackGenerator().notificationOccurred(.success)
                    finished = true; scanner.stopScanning(); result(code); return
                }
                sawInvalidQR = true
            }
            let invalidHint = "请扫描电脑「设置 → 远程连接」中的 Super Cage 配对二维码"
            if sawInvalidQR, hint.wrappedValue != invalidHint {
                hint.wrappedValue = invalidHint
            }
        }
        func dataScanner(_ scanner: DataScannerViewController, didAdd items: [RecognizedItem], allItems: [RecognizedItem]) {
            handle(allItems, in: scanner)
        }
        func dataScanner(_ scanner: DataScannerViewController, didUpdate items: [RecognizedItem], allItems: [RecognizedItem]) {
            handle(allItems, in: scanner)
        }
        func dataScanner(_ scanner: DataScannerViewController, becameUnavailableWithError error: DataScannerViewController.ScanningUnavailable) {
            if !finished {finished = true; result(nil)}
        }
    }
    func makeCoordinator() -> Delegate { Delegate(result, hint: $hint) }
    func makeUIViewController(context: Context) -> DataScannerViewController {
        let scanner = DataScannerViewController(recognizedDataTypes: [.barcode(symbologies: [.qr])],
                                                qualityLevel: .accurate,
                                                recognizesMultipleItems: true,
                                                isPinchToZoomEnabled: true,
                                                isGuidanceEnabled: false,
                                                isHighlightingEnabled: false)
        scanner.delegate = context.coordinator
        DispatchQueue.main.async {
            guard !context.coordinator.finished, !scanner.isScanning else {return}
            do { try scanner.startScanning() } catch {context.coordinator.finished = true; result(nil)}
        }
        return scanner
    }
    func updateUIViewController(_ scanner: DataScannerViewController, context: Context) {
        guard !context.coordinator.finished, !scanner.isScanning, scanner.view.window != nil else {return}
        try? scanner.startScanning()
    }
    static func dismantleUIViewController(_ scanner: DataScannerViewController, coordinator: Delegate) {coordinator.finished = true; scanner.stopScanning()}
}

private struct FinderMask: Shape {
    let box: CGRect
    func path(in rect: CGRect) -> Path {
        var path = Path(rect)
        path.addRoundedRect(in: box, cornerSize: CGSize(width: 18, height: 18))
        return path
    }
}
private struct FinderCorners: Shape {
    func path(in rect: CGRect) -> Path {
        var path = Path()
        for x in [CGFloat(0), CGFloat(1)] {for y in [CGFloat(0), CGFloat(1)] {
            var corner = Path()
            corner.move(to: CGPoint(x: 0, y: 30)); corner.addLine(to: CGPoint(x: 0, y: 18))
            corner.addQuadCurve(to: CGPoint(x: 18, y: 0), control: .zero); corner.addLine(to: CGPoint(x: 30, y: 0))
            let transform = CGAffineTransform(a: x == 0 ? 1 : -1, b: 0, c: 0, d: y == 0 ? 1 : -1, tx: x * rect.width, ty: y * rect.height)
            path.addPath(corner, transform: transform)
        }}
        return path
    }
}
private struct QRScannerScreen: View {
    let result: (String?) -> Void
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var hint = "将 Super Cage 配对二维码放入框内\n在电脑「设置 → 远程连接」中生成"
    private let accent = CageStyle.clay
    private let night = Color(red: 0x1F / 255, green: 0x1E / 255, blue: 0x1D / 255)
    private let cream = Color(red: 0xF3 / 255, green: 0xEF / 255, blue: 0xE5 / 255)
    var body: some View {
        GeometryReader { geometry in
            let side = max(96, min(280, min(geometry.size.width - 56, geometry.size.height - 190)))
            let box = CGRect(x: (geometry.size.width - side) / 2, y: 24 + max(0, (geometry.size.height - 190 - side) / 2), width: side, height: side)
            ZStack(alignment: .bottom) {
                #if targetEnvironment(simulator)
                night.opacity(0.9)
                Image(systemName: "qrcode").resizable().interpolation(.none).scaledToFit()
                    .foregroundStyle(cream.opacity(0.8)).frame(width: side * 0.65, height: side * 0.65)
                    .position(x: box.midX, y: box.midY).accessibilityHidden(true)
                #else
                QRScanner(hint: $hint, result: result)
                #endif
                FinderMask(box: box).fill(night.opacity(0.74), style: FillStyle(eoFill: true))
                    .allowsHitTesting(false)
                RoundedRectangle(cornerRadius: 18).stroke(cream.opacity(0.4), lineWidth: 1)
                    .frame(width: side, height: side).position(x: box.midX, y: box.midY)
                    .allowsHitTesting(false)
                FinderCorners().stroke(accent, style: StrokeStyle(lineWidth: 4, lineCap: .round))
                    .frame(width: side, height: side).position(x: box.midX, y: box.midY)
                    .allowsHitTesting(false)
                // The sweep follows the clock instead of an implicit repeating animation, so a box that is still
                // moving (sheet push, navigation bar) never leaves a stale animation behind. Clipped to the box.
                TimelineView(.animation(paused: reduceMotion)) { context in
                    // Linear ping-pong 0…1…0, 2.4 s each way.
                    let cycle = (context.date.timeIntervalSinceReferenceDate / 2.4).truncatingRemainder(dividingBy: 2)
                    let phase = reduceMotion ? 0.5 : CGFloat(cycle < 1 ? cycle : 2 - cycle)
                    VStack(spacing: 0) {
                        LinearGradient(colors: [accent.opacity(0), accent.opacity(0.22)], startPoint: .top, endPoint: .bottom).frame(height: 26)
                        Capsule().fill(accent).frame(height: 2)
                    }.frame(width: side - 24)
                        .position(x: side / 2, y: 24 + phase * (side - 48))
                }.frame(width: side, height: side).clipShape(RoundedRectangle(cornerRadius: 18))
                    .position(x: box.midX, y: box.midY)
                    .allowsHitTesting(false)
                    .accessibilityHidden(true)
                VStack(spacing: 12) {
                    Text("连接你的电脑").font(CageStyle.serif(24))
                    Text(hint).font(.subheadline).foregroundStyle(cream.opacity(0.88)).multilineTextAlignment(.center)
                    Text(simulatorPreviewEnabled ? "模拟器预览 · 即将进入示例工作台" : "仅识别 Super Cage 配对码")
                        .font(.caption).foregroundStyle(Color(red: 0xAB / 255, green: 0xA5 / 255, blue: 0x97 / 255))
                }.foregroundStyle(cream).padding(.horizontal, 24).padding(.bottom, 24).allowsHitTesting(false).accessibilityElement(children: .combine)
            }.background(night)
                .task {
                    #if targetEnvironment(simulator)
                    do {try await Task.sleep(for: .seconds(2))} catch {return}
                    result(nil)
                    #endif
                }
        }.toolbarColorScheme(.dark, for: .navigationBar)
            .toolbarBackground(night, for: .navigationBar).toolbarBackground(.visible, for: .navigationBar)
    }
}

private struct ConnectionSheet: View {
    @ObservedObject var model: ConnectionModel
    let previewConnected: () -> Void
    @State private var manual = false
    @State private var scanning = false
    @State private var disconnecting: ConnectedDevice?
    @State private var previewScanned = false
    @State private var code = ""
    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(spacing: 12) {
                    Image("DoodleLaptop").resizable().scaledToFit().frame(height: 116).accessibilityHidden(true)
                    Text("连接电脑").font(CageStyle.serif(28)).foregroundStyle(CageStyle.ink).accessibilityAddTraits(.isHeader)
                    Text("在电脑的 Super Cage 设置中打开「远程连接」，生成配对二维码或复制配对码。")
                        .font(.subheadline).foregroundStyle(CageStyle.ink2).multilineTextAlignment(.center)
                        .padding(.bottom, 10)
                    Button {
                        Task { @MainActor in
                            if simulatorPreviewEnabled {scanning = true; return}
                            guard DataScannerViewController.isSupported,
                                  await AVCaptureDevice.requestAccess(for: .video),
                                  DataScannerViewController.isAvailable else {
                                model.status = "相机暂不可用，请检查相机权限或使用配对码。"; return
                            }
                            scanning = true
                        }
                    } label: {ActionLabel(title: "扫码连接", symbol: "qrcode.viewfinder")}
                    Button {manual = true} label: {ActionLabel(title: "使用配对码", symbol: "key", primary: false)}
                    if model.hasSaved || model.hasInvitation {
                        Button(action: model.reconnectSaved) {
                            Text("重新连接已配对的电脑").font(.body.weight(.medium)).foregroundStyle(CageStyle.clayInk)
                                .frame(maxWidth: .infinity, minHeight: 44).contentShape(Rectangle())
                        }.disabled(!model.canConnect)
                    }
                    if !model.devices.isEmpty {
                        VStack(alignment: .leading, spacing: 8) {
                            Text("已连接设备").font(.footnote.weight(.medium)).foregroundStyle(CageStyle.ink2)
                            ForEach(model.devices) {device in
                                Button {if disconnecting == nil {model.selectDevice(device.id)}} label: {
                                    HStack(spacing: 12) {
                                        Image(systemName: device.account == nil ? "laptopcomputer" : "server.rack")
                                            .foregroundStyle(CageStyle.ink2).frame(width: 26).accessibilityHidden(true)
                                        Text(device.name).foregroundStyle(CageStyle.ink).lineLimit(1)
                                        Spacer()
                                        Image(systemName: "chevron.right").font(.footnote.weight(.semibold)).foregroundStyle(CageStyle.ink2)
                                    }.padding(.horizontal, 14).frame(minHeight: 52)
                                        .background(CageStyle.surface2, in: RoundedRectangle(cornerRadius: 12))
                                        .contentShape(Rectangle())
                                }
                                .simultaneousGesture(LongPressGesture().onEnded { _ in disconnecting = device })
                                .accessibilityAction(named: Text("取消连接")) {disconnecting = device}
                            }
                            Text("长按设备可取消连接").font(.footnote).foregroundStyle(CageStyle.ink2)
                        }.frame(maxWidth: .infinity, alignment: .leading).padding(.top, 16)
                    }
                    status
                }.padding(24).frame(maxWidth: 520)
                    .frame(maxWidth: .infinity)
            }.background(CageStyle.surface).buttonStyle(CageButtonStyle())
                .navigationDestination(isPresented: $manual) {pairCode}
                .sheet(item: $disconnecting) {device in
                    VStack(spacing: 14) {
                        Text("取消连接？").font(CageStyle.serif(24)).foregroundStyle(CageStyle.ink)
                        Text(device.name).foregroundStyle(CageStyle.ink)
                        Text("将关闭这台设备的连接。已保存的配对信息会保留。")
                            .font(.subheadline).foregroundStyle(CageStyle.ink2).multilineTextAlignment(.center)
                        Button {
                            model.disconnectDevice(device.id)
                            disconnecting = nil
                        } label: {ActionLabel(title: "取消连接", danger: true)}.padding(.top, 6)
                        Button {disconnecting = nil} label: {ActionLabel(title: "保留连接", primary: false)}
                    }.padding(24).buttonStyle(CageButtonStyle())
                        .presentationDetents([.height(330)]).presentationDragIndicator(.visible)
                        .presentationBackground(CageStyle.surface)
                }
                .sheet(isPresented: $scanning, onDismiss: {
                    if previewScanned {previewScanned = false; previewConnected()}
                }) {
                    NavigationStack {
                        QRScannerScreen { result in
                            scanning = false
                            if simulatorPreviewEnabled {previewScanned = true}
                            else if let result {model.pair(code: result)}
                            else {model.status = "无法读取二维码，请重试或使用配对码。"}
                        }.navigationTitle("扫描电脑二维码").navigationBarTitleDisplayMode(.inline)
                        .toolbar {Button("取消") {scanning = false}}
                    }
                }
        }.disabled(model.connecting).interactiveDismissDisabled(model.connecting)
            .presentationDetents([.fraction(0.72), .large]).presentationDragIndicator(.visible)
            .presentationBackground(CageStyle.surface)
    }
    private var pairCode: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                Text("粘贴电脑上复制的一次性配对码。").font(.subheadline).foregroundStyle(CageStyle.ink2)
                SecureField("粘贴电脑上的一次性配对码", text: $code)
                    .textInputAutocapitalization(.never).autocorrectionDisabled().cageField()
                Button {model.pair(code: code); code = ""} label: {ActionLabel(title: "连接电脑")}
                    .accessibilityIdentifier("redeem-pair-code")
                    .disabled(code.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || model.connecting)
                status
            }.padding(24).frame(maxWidth: 520).frame(maxWidth: .infinity)
        }.background(CageStyle.surface).buttonStyle(CageButtonStyle())
            .navigationTitle("配对码").navigationBarTitleDisplayMode(.inline)
    }
    @ViewBuilder private var status: some View {
        if !model.status.isEmpty {
            Text(model.status).font(.footnote).foregroundStyle(CageStyle.ink2)
                .accessibilityAddTraits(.updatesFrequently)
        }
    }
}

private enum CageTheme: String, CaseIterable {
    case system = "跟随系统", light = "浅色", dark = "深色"
    var scheme: ColorScheme? {
        switch self {case .system: return nil; case .light: return .light; case .dark: return .dark}
    }
    @ViewBuilder var art: some View {
        switch self {
        case .system:
            HStack(spacing: -18) {Image("DoodleSun").resizable().scaledToFit(); Image("DoodleMoon").resizable().scaledToFit()}
        case .light: Image("DoodleSun").resizable().scaledToFit()
        case .dark: Image("DoodleMoon").resizable().scaledToFit()
        }
    }
}

/// Three cards for 跟随系统 / 浅色 / 深色, the chosen one outlined in clay.
private struct ThemePicker: View {
    @Binding var theme: CageTheme
    var body: some View {
        HStack(spacing: 10) {
            ForEach(CageTheme.allCases, id: \.self) {option in
                let chosen = theme == option
                Button {theme = option} label: {
                    VStack(spacing: 4) {
                        option.art.frame(height: 40).accessibilityHidden(true)
                        Text(option.rawValue).font(.subheadline).foregroundStyle(CageStyle.ink)
                    }.frame(maxWidth: .infinity, minHeight: 88)
                        .background(chosen ? CageStyle.surface2 : CageStyle.surface, in: RoundedRectangle(cornerRadius: 12))
                        .overlay(RoundedRectangle(cornerRadius: 12).strokeBorder(chosen ? CageStyle.clay : CageStyle.line, lineWidth: chosen ? 1.5 : 1))
                        .contentShape(Rectangle())
                }.buttonStyle(CageButtonStyle())
                    .accessibilityLabel("主题：" + option.rawValue)
                    .accessibilityAddTraits(chosen ? .isSelected : [])
            }
        }
    }
}

private struct SettingsScreen: View {
    @ObservedObject var model: ConnectionModel
    @Binding var theme: CageTheme
    let connect: () -> Void
    @State private var relaySettings = false
    @State private var advanced = false
    @State private var picking = false
    @State private var confirmingForget = false

    var body: some View {
        Form {
            Section("外观") {
                ThemePicker(theme: $theme).listRowInsets(EdgeInsets(top: 12, leading: 12, bottom: 12, trailing: 12))
            }.listRowBackground(CageStyle.surface)
            Section("电脑连接") {
                Button("已归档会话", action: model.openArchivedSessions)
                    .disabled(model.selectedDevice == nil && !model.canConnect)
                DisclosureGroup("中继设置", isExpanded: $relaySettings) {
                    Toggle("使用自建中继", isOn: Binding(get: {model.relay != CMConnection.defaultRelay()},
                                                     set: {model.relay = $0 ? "" : CMConnection.defaultRelay()}))
                        .tint(CageStyle.clay)
                    if model.relay != CMConnection.defaultRelay() {
                        TextField("自建 VPS 的 HTTPS 地址", text: $model.relay)
                            .keyboardType(.URL).textInputAutocapitalization(.never).autocorrectionDisabled()
                    } else {Text("Super Cage 官方中继").foregroundStyle(CageStyle.ink2)}
                    Button("保存中继设置", action: model.saveRelay).foregroundStyle(CageStyle.clayInk)
                }
                DisclosureGroup("指纹与连接文件", isExpanded: $advanced) {
                    Text(CMConnection.usesSimulatorIdentity ? "模拟器测试身份的指纹" : "这台手机的指纹").font(.headline)
                    Text(model.fingerprint).font(.caption.monospaced()).foregroundStyle(CageStyle.ink2).textSelection(.enabled)
                    Button("复制手机指纹") {UIPasteboard.general.string = model.fingerprint; model.status = "手机指纹已复制"}
                        .foregroundStyle(CageStyle.clayInk).disabled(model.fingerprint.isEmpty)
                    Button("导入连接文件") {picking = true}.foregroundStyle(CageStyle.clayInk)
                    TextField("电脑指纹 sha256:…", text: $model.pin).textInputAutocapitalization(.never).autocorrectionDisabled()
                    Toggle("已核对电脑指纹", isOn: $model.verified).tint(CageStyle.clay)
                    Button("保存已核对的主机", action: model.saveTrust).foregroundStyle(CageStyle.clayInk).disabled(!model.verified)
                    if model.hasInvitation || model.hasSaved {
                        Button("连接工作台", action: connect).foregroundStyle(CageStyle.clayInk).disabled(!model.canConnect)
                    }
                }
                if model.canForget {Button("忘记主机", role: .destructive) {confirmingForget = true}.foregroundStyle(CageStyle.danger)}
            }.listRowBackground(CageStyle.surface)
            if !model.status.isEmpty {
                Section {Text(model.status).font(.footnote).foregroundStyle(CageStyle.ink2)}.listRowBackground(Color.clear)
            }
        }.cageForm().navigationTitle("设置").disabled(model.connecting)
            .alert("忘记主机？", isPresented: $confirmingForget) {
                Button("取消", role: .cancel) {}
                Button("忘记主机", role: .destructive, action: model.forget)
            } message: {Text("将移除这台电脑的地址、指纹和重连凭据。")}
            .fileImporter(isPresented: $picking, allowedContentTypes: [.json, .plainText]) {result in
                if case .success(let file) = result {model.loadInvitation(file)}
            }
    }
}

@main
struct CageMobileApp: App {
    @Environment(\.scenePhase) private var scenePhase
    @StateObject private var model = ConnectionModel()
    @StateObject private var servers = ServerModel()
    @AppStorage("appearance-theme") private var theme = CageTheme.system
    @State private var showingSettings = false
    @State private var pairing = false
    @State private var serverSheet = false

    init() {
        // Serif navigation titles; colours and bar backgrounds stay with the system.
        func serif(_ size: CGFloat, _ weight: UIFont.Weight) -> UIFont {
            let font = UIFont.systemFont(ofSize: size, weight: weight)
            return UIFont(descriptor: font.fontDescriptor.withDesign(.serif) ?? font.fontDescriptor, size: size)
        }
        UINavigationBar.appearance().titleTextAttributes = [.font: serif(18, .medium)]
        UINavigationBar.appearance().largeTitleTextAttributes = [.font: serif(34, .regular)]
    }

    var body: some Scene {
        WindowGroup {
            NavigationStack {
                Group {
                    if model.showingWorkspace, let device = model.selectedDevice {
                        Workspace(webView: device.webView).id(device.id)
                            .background(CageStyle.paper.ignoresSafeArea())
                    } else {
                        home
                    }
                }
                .navigationDestination(isPresented: $showingSettings) {
                    SettingsScreen(model: model, theme: $theme, connect: model.connect)
                }
            }.buttonStyle(CageButtonStyle()).tint(CageStyle.ink).preferredColorScheme(theme.scheme)
                .sheet(isPresented: $pairing) {
                    ConnectionSheet(model: model, previewConnected: {
                        pairing = false; showingSettings = false; model.startPreview()
                    }).buttonStyle(CageButtonStyle()).tint(CageStyle.ink).preferredColorScheme(theme.scheme)
                }
                .sheet(isPresented: $serverSheet) {
                    ServerSheet(model: servers, connections: model)
                        .tint(CageStyle.ink).preferredColorScheme(theme.scheme)
                }
                .onAppear {
                    servers.connections = model
                    #if targetEnvironment(simulator)
                    // Screenshot hook: xcrun simctl launch … -preview-screen pair|servers|settings|workspace
                    switch UserDefaults.standard.string(forKey: "preview-screen") {
                    case "pair": pairing = true
                    case "servers": serverSheet = true
                    case "settings": showingSettings = true
                    case "workspace": model.startPreview()
                    default: break
                    }
                    #endif
                }
                .onChange(of: model.showingWorkspace) { _, connected in
                    if connected {pairing = false; showingSettings = false; serverSheet = false}
                }
                .onChange(of: model.relay) {model.verified = false}
                .onChange(of: model.pin) {model.verified = false}
                .onChange(of: scenePhase) { _, phase in
                    if phase == .background {model.handleBackground()}
                    else if phase == .active {model.handleForeground()}
                }
        }
    }
    private var home: some View {
        GeometryReader { geometry in
            let brandSpacing = min(56, max(28, geometry.size.height * 0.07)) + 16
            ScrollView {
                VStack(spacing: 0) {
                    Spacer(minLength: 12)
                    VStack(spacing: brandSpacing) {
                        VStack(spacing: 0) {
                            Image("Logo").resizable().scaledToFit().frame(width: 96, height: 96).accessibilityHidden(true)
                            Text("Super Cage").font(CageStyle.serif(36)).foregroundStyle(CageStyle.ink)
                                .padding(.top, 12).accessibilityAddTraits(.isHeader)
                            Image("DoodleSquiggle").resizable().frame(width: 150, height: 12).accessibilityHidden(true)
                            Text("在手机上接着写代码").font(.system(size: 16)).foregroundStyle(CageStyle.ink2).padding(.top, 14)
                        }
                        VStack(spacing: 12) {
                            Button {pairing = true} label: {ActionLabel(title: "连接电脑", symbol: "laptopcomputer")}
                            Button {serverSheet = true} label: {ActionLabel(title: "连接服务器", symbol: "server.rack", primary: false)}
                            Button {model.openExample()} label: {
                                VStack(spacing: 2) {
                                    Text("看看示例工作台").font(.body.weight(.medium)).foregroundStyle(CageStyle.clayInk)
                                    Text("不需要账号，使用演示数据").font(.footnote).foregroundStyle(CageStyle.ink2)
                                }.frame(maxWidth: .infinity, minHeight: 56).contentShape(Rectangle())
                            }
                        }
                    }.frame(maxWidth: 300).padding(.top, 24)
                    Spacer(minLength: 12)
                }.padding(.horizontal, 24).frame(maxWidth: .infinity).frame(minHeight: geometry.size.height)
            }.scrollBounceBehavior(.basedOnSize)
        }.background(CageStyle.paper.ignoresSafeArea()).buttonStyle(CageButtonStyle())
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button {showingSettings = true} label: {Image(systemName: "gearshape")}
                        .accessibilityLabel("设置")
                }
            }
    }
}
