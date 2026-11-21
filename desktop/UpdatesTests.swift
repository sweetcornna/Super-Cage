import Cocoa

@main
enum UpdatesTests {
    static func expect(_ condition: @autoclosure () -> Bool, _ message: String) throws {
        if !condition() { throw CageUpdateError("TEST FAILED: " + message) }
    }
    static func rejects(_ message: String, _ body: () throws -> Void) throws {
        var rejected = false
        do { try body() } catch { rejected = true }
        try expect(rejected, message)
    }
    static func main() throws {
        try expect(CageUpdateVersion.isNewer("0.2.0-alpha.10", than: "0.2.0-alpha.9"), "numeric prerelease ordering")
        try expect(CageUpdateVersion.isNewer("0.2.0", than: "0.2.0-alpha.10"), "stable promotion")
        for invalid in ["0.2.0-alpha.01", "../0.3.0", "1.0", "1.0.0/../../"] {
            try expect(!CageUpdateVersion.isNewer(invalid, than: "0.1.0"), "invalid version rejected")
        }
        try expect(!CageUpdateVersion.isNewer("0.2.0", than: "0.2.0"), "same version rejected")
        try expect(!CageUpdateVersion.isNewer("0.1.0", than: "0.2.0"), "downgrade rejected")

        let root = FileManager.default.temporaryDirectory.appendingPathComponent("cage-updates-test-" + UUID().uuidString)
        try CageUpdateFiles.privateDirectory(root)
        defer { try? FileManager.default.removeItem(at: root) }
        do {
            let saved = ProcessInfo.processInfo.environment["CLAUDE_CAGE_CONFIG"]
            let directory = FileManager.default.currentDirectoryPath
            defer {
                _ = FileManager.default.changeCurrentDirectoryPath(directory)
                if let saved { setenv("CLAUDE_CAGE_CONFIG", saved, 1) } else { unsetenv("CLAUDE_CAGE_CONFIG") }
            }
            setenv("CLAUDE_CAGE_CONFIG", "", 1)
            try expect(CageProfile.configURL() == FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".claude-cage/config.json"), "empty profile agrees with backend default")
            try expect(FileManager.default.changeCurrentDirectoryPath(root.path), "enter isolated launch directory")
            let launchDirectory = FileManager.default.currentDirectoryPath
            setenv("CLAUDE_CAGE_CONFIG", "selected profile/config.json", 1)
            CageProfile.pinEnvironment()
            let pinned = CageProfile.configURL()
            try expect(pinned.path == launchDirectory + "/selected profile/config.json", "relative profile pinned at launch")
            try expect(FileManager.default.changeCurrentDirectoryPath(directory), "simulate helper working directory")
            try expect(CageProfile.configURL() == pinned, "helper keeps original profile after cwd changes")
            let inherited = try CageUpdateFiles.run("/bin/sh", ["-c", "printf '%s' \"$CLAUDE_CAGE_CONFIG\""])
            try expect(String(data: inherited, encoding: .utf8) == pinned.path, "actual child process inherits absolute profile")
        }
        let personalFiles = [
            "account.json", "favorites.json", "projects.json", "appearance.json", "workbench.json",
            "state/lima/persistent/disk", "webview/data/Local Storage/leveldb/000001.log",
            "workspace/project.txt"
        ]
        let profile = root.appendingPathComponent("profile")
        var personalData: [String: Data] = [:]
        for name in personalFiles {
            let file = profile.appendingPathComponent(name)
            try CageUpdateFiles.privateDirectory(file.deletingLastPathComponent())
            let bytes = Data("Keep \(name): 历史、收藏和登录状态\n\0".utf8)
            try CageUpdateFiles.write(bytes, to: file)
            personalData[name] = bytes
        }
        func verifyPersonalData() throws {
            for (name, expected) in personalData {
                let actual = try Data(contentsOf: profile.appendingPathComponent(name))
                try expect(actual == expected, "application replacement or rollback changed personal data: \(name)")
            }
        }
        // Exercise the production lipo invocation against a real Mach-O binary.
        // lipo consumes every argument after -verify_arch as an architecture.
        try CageUpdateFiles.verifyArchitecture(URL(fileURLWithPath: CommandLine.arguments[0]))
        let hashFile = root.appendingPathComponent("content")
        try CageUpdateFiles.write(Data("abc".utf8), to: hashFile)
        try rejects("non-Mach-O executable refused") { try CageUpdateFiles.verifyArchitecture(hashFile) }
        let hash = try CageUpdateFiles.digest(hashFile)
        try expect(hash == "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", "stream digest")
        let permissions = try FileManager.default.attributesOfItem(atPath: hashFile.path)
        try expect((permissions[.posixPermissions] as? NSNumber)?.intValue == 0o600, "private file mode")
        let escaped = root.appendingPathComponent("escaped.app")
        try CageUpdateFiles.privateDirectory(escaped)
        try FileManager.default.createSymbolicLink(at: escaped.appendingPathComponent("outside"), withDestinationURL: hashFile)
        try rejects("app symlink escape") { try CageUpdateFiles.validateTree(escaped) }

        let image = root.appendingPathComponent("image")
        try CageUpdateFiles.privateDirectory(image)
        try rejects("image without an app refused") { _ = try CageUpdateFiles.imageApp(in: image) }
        try CageUpdateFiles.privateDirectory(image.appendingPathComponent("Cage.app"))
        let legacyApp = try CageUpdateFiles.imageApp(in: image)
        try expect(legacyApp.lastPathComponent == "Cage.app", "image from before the rename")
        try CageUpdateFiles.privateDirectory(image.appendingPathComponent("Super Cage.app"))
        let renamedApp = try CageUpdateFiles.imageApp(in: image)
        try expect(renamedApp.lastPathComponent == "Super Cage.app", "renamed app preferred")

        let old = root.appendingPathComponent("Cage.app"), next = root.appendingPathComponent("next.app"), backup = root.appendingPathComponent("backup.app")
        for location in [old, next, backup] { try CageUpdateFiles.privateDirectory(location) }
        try Data("old".utf8).write(to: old.appendingPathComponent("marker"))
        try Data("old".utf8).write(to: backup.appendingPathComponent("marker"))
        try Data("new".utf8).write(to: next.appendingPathComponent("marker"))
        try CageUpdateFiles.replaceAndVerify(target: old, candidate: next, backup: backup, manager: .default) {
            let marker = try String(contentsOf: $0.appendingPathComponent("marker"), encoding: .utf8)
            try expect(marker == "new", "new content reaches final install path")
        }
        try verifyPersonalData()
        try CageUpdateFiles.restore(backup: backup, target: old, manager: .default)
        try verifyPersonalData()
        let restored = try String(contentsOf: old.appendingPathComponent("marker"), encoding: .utf8)
        try expect(restored == "old", "launch failure can restore original")
        try CageUpdateFiles.privateDirectory(next)
        try Data("invalid".utf8).write(to: next.appendingPathComponent("marker"))
        try rejects("post-install validation must roll back") {
            try CageUpdateFiles.replaceAndVerify(target: old, candidate: next, backup: backup, manager: .default) { _ in throw CageUpdateError("verification failed") }
        }
        let rollback = try String(contentsOf: old.appendingPathComponent("marker"), encoding: .utf8)
        try expect(rollback == "old", "failed validation leaves original installed")
        try verifyPersonalData()
        try CageUpdateFiles.privateDirectory(next)
        try Data("invalid-after-replace".utf8).write(to: next.appendingPathComponent("marker"))
        var replaced = false
        try rejects("post-install and rollback failure must remain a changed transaction") {
            try CageUpdateFiles.replaceAndVerify(target: old, candidate: next, backup: backup, manager: .default,
                onReplace: { replaced = true }, restoreOriginal: { throw CageUpdateError("injected restore I/O failure") }) { _ in
                    throw CageUpdateError("injected validation failure")
                }
        }
        try expect(replaced, "replacement must be recorded before validation or rollback can fail")
        let failedTarget = try String(contentsOf: old.appendingPathComponent("marker"), encoding: .utf8)
        let retainedBackup = try String(contentsOf: backup.appendingPathComponent("marker"), encoding: .utf8)
        try expect(failedTarget == "invalid-after-replace" && retainedBackup == "old", "double failure must retain the original backup and report the changed target")
        try CageUpdateFiles.restore(backup: backup, target: old, manager: .default)
        try verifyPersonalData()
        try rejects("unsigned application refused") { _ = try CageUpdateFiles.signingInfo(old, bundle: true) }

        // A real child that ignores SIGTERM must not keep cancellation pending.
        let running = root.appendingPathComponent("validation-running")
        let task = CageUpdateValidation()
        DispatchQueue.global().async {
            defer { task.finish() }
            _ = try? CageUpdateFiles.run("/bin/sh", ["-c", "trap '' TERM; touch \"$1\"; exec /bin/sleep 30", "validation", running.path], validation: task)
        }
        let startedDeadline = Date().addingTimeInterval(5)
        while !FileManager.default.fileExists(atPath: running.path) && Date() < startedDeadline { Thread.sleep(forTimeInterval: 0.01) }
        try expect(FileManager.default.fileExists(atPath: running.path), "real validation child started")
        let cancelledAt = Date()
        task.cancel(); task.wait()
        try expect(Date().timeIntervalSince(cancelledAt) < 5, "unresponsive verification process is killed and joined")

        // Mount only this test's own tiny image. Cancellation inside the real
        // mount scope must still execute hdiutil detach before completion.
        let contents = root.appendingPathComponent("image-source")
        try CageUpdateFiles.privateDirectory(contents)
        try Data("fixture".utf8).write(to: contents.appendingPathComponent("marker"))
        let archive = root.appendingPathComponent("fixture.dmg")
        _ = try CageUpdateFiles.run("/usr/bin/hdiutil", ["create", "-size", "20m", "-fs", "HFS+", "-volname", "CageUpdateQA", "-srcfolder", contents.path, archive.path])
        let mountTask = CageUpdateValidation()
        try rejects("cancelled mounted verification") {
            try CageUpdateFiles.withMountedImage(archive: archive, directory: root.appendingPathComponent("verification"), validation: mountTask) { mount in
                try expect(FileManager.default.fileExists(atPath: mount.appendingPathComponent("marker").path), "test image mounted")
                mountTask.cancel()
                try mountTask.checkCancellation()
            }
        }
        mountTask.finish(); mountTask.wait()
        let mounts = try CageUpdateFiles.run("/usr/bin/hdiutil", ["info", "-plist"])
        let info = try PropertyListSerialization.propertyList(from: mounts, format: nil) as? [String: Any]
        let images = info?["images"] as? [[String: Any]] ?? []
        try expect(!images.contains(where: { $0["image-path"] as? String == archive.path }), "cancellation detached its own image")

        // A just-installed instance can be rolled back natively until the
        // trusted workbench explicitly confirms its API initialization.
        let savedEnvironment = ProcessInfo.processInfo.environment
        let confirmation = UUID().uuidString
        setenv("CAGE_UPDATE_CONFIRMATION", confirmation, 1)
        setenv("CLAUDE_CAGE_CONFIG", root.appendingPathComponent("profile/config.json").path, 1)
        defer {
            for key in ["CAGE_UPDATE_CONFIRMATION", "CLAUDE_CAGE_CONFIG"] {
                if let value = savedEnvironment[key] { setenv(key, value, 1) } else { unsetenv(key) }
            }
        }
        let updater = CageAppUpdater(bundleURL: old, onChange: { _ in }, quit: {}, reconnect: {})
        try expect(updater.awaitingInitialWorkbench, "newly installed uninitialized window permits native rollback termination")
        updater.markLaunchReady()
        try expect(!updater.awaitingInitialWorkbench, "initialized workbench keeps ordinary quit confirmation")
        try expect(FileManager.default.fileExists(atPath: root.appendingPathComponent("profile/updates/launch-\(confirmation).json").path), "explicit workbench readiness writes installer acknowledgement")

        let probeBundle = root.appendingPathComponent("retry-protocol.app")
        let resources = probeBundle.appendingPathComponent("Contents/Resources")
        try CageUpdateFiles.privateDirectory(resources)
        let countFile = root.appendingPathComponent("retry-invocations")
        let retryAt = ISO8601DateFormatter().string(from: Date().addingTimeInterval(3600))
        let script = "#!/bin/sh\nprintf x >> '\(countFile.path)'\nprintf '%s\\n' '{\"phase\":\"failed\",\"message\":\"limited\",\"retryable\":true,\"retryAt\":\"\(retryAt)\"}'\nexit 1\n"
        let probe = resources.appendingPathComponent("cage")
        try Data(script.utf8).write(to: probe)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: probe.path)
        #if arch(x86_64)
        // Real Intel bundles must never invoke the ARM64-only update channel.
        // The standalone process fixture below still verifies its retry protocol.
        for suffix in ["", ".intel-macos-test"] {
            let manualBundle = root.appendingPathComponent("manual\(suffix).app")
            let manualResources = manualBundle.appendingPathComponent("Contents/Resources")
            try CageUpdateFiles.privateDirectory(manualResources)
            try FileManager.default.copyItem(at: probe, to: manualResources.appendingPathComponent("cage"))
            let info: [String: Any] = ["CFBundleIdentifier": cageUpdateBundleID + suffix, "CFBundlePackageType": "APPL", "CageVersion": "0.0.0"]
            let plist = try PropertyListSerialization.data(fromPropertyList: info, format: .xml, options: 0)
            try plist.write(to: manualBundle.appendingPathComponent("Contents/Info.plist"))
            let manual = CageAppUpdater(bundleURL: manualBundle, onChange: { _ in }, quit: {}, reconnect: {})
            for action in ["check", "install", "status"] {
                try expect(manual.handle(action)["phase"] as? String == "manual", "Intel bundle uses independent package updates")
            }
            try expect(!FileManager.default.fileExists(atPath: countFile.path), "Intel bundle launched the incompatible update channel")
        }
        #endif
        var received: [String: Any] = [:]
        let limited = CageAppUpdater(bundleURL: probeBundle, onChange: { received = $0 }, quit: {}, reconnect: {})
        _ = limited.handle("check")
        let deadline = Date().addingTimeInterval(5)
        while received["phase"] as? String != "failed" && Date() < deadline { _ = RunLoop.current.run(mode: .default, before: Date().addingTimeInterval(0.01)) }
        // Drain the process-exit callback as well as the streamed failed state.
        _ = RunLoop.current.run(mode: .default, before: Date().addingTimeInterval(0.1))
        try expect(limited.handle("status")["retryAt"] as? String == retryAt, "native process-exit handling retained server retry deadline")
        _ = limited.handle("check"); _ = limited.handle("install")
        let invocationData = try Data(contentsOf: countFile)
        try expect(String(data: invocationData, encoding: .utf8) == "x", "native menu or install bypassed cooldown")
        print("PASS: native update failure retained retry deadline and blocked early check/install")

        // The workbench decides "may I install now?"; the shell only passes on
        // what the person confirmed or the workbench's own silent request.
        try expect(CageUpdateDecision.arguments(version: "0.1.16", silentRequest: nil, confirmed: ["terminal-command:7", "bad,id", "agent:claude"]) ==
            ["prepare-update", "--stop", "--json", "--app-update", "0.1.16", "--trigger", "click", "--confirmed", "terminal-command:7,agent:claude"], "click arguments")
        try expect(CageUpdateDecision.arguments(version: "0.1.16", silentRequest: "0123456789abcdef", confirmed: []) ==
            ["prepare-update", "--stop", "--json", "--app-update", "0.1.16", "--trigger", "silent", "--request", "0123456789abcdef"], "silent arguments")
        try expect(CageUpdateDecision.outcome(Data(#"{"status":"prepared","attempt":"0123456789abcdef"}"#.utf8), exitStatus: 0) == .prepared(attempt: "0123456789abcdef"), "prepared")
        try expect(CageUpdateDecision.outcome(Data(#"{"status":"prepared","attempt":"../x"}"#.utf8), exitStatus: 0) == .prepared(attempt: ""), "invalid attempt dropped")
        try expect(CageUpdateDecision.outcome(Data(#"{"status":"blocked","code":"must_wait","message":"以下操作完成后才能升级：「更新环境组件」正在进行"}"#.utf8), exitStatus: 0) ==
            .blocked(code: "must_wait", message: "以下操作完成后才能升级：「更新环境组件」正在进行", blockers: []), "blocked")
        try expect(CageUpdateDecision.outcome(Data(#"{"status":"blocked","code":"confirm_required","message":"m","blockers":[{"id":"terminal-command:7","label":"终端里还有命令在运行（make）","interruptible":true},{"id":"bad id","label":"x"},{"id":"unknown:terminals","label":"无法确认","interruptible":true,"unknown":true}]}"#.utf8), exitStatus: 0) ==
            .blocked(code: "confirm_required", message: "m", blockers: [.init(id: "terminal-command:7", label: "终端里还有命令在运行（make）", interruptible: true, unknown: false), .init(id: "unknown:terminals", label: "无法确认", interruptible: true, unknown: true)]), "blocked names what to confirm and drops invalid ids")
        try expect(CageUpdateDecision.outcome(Data(#"{"status":"prepared"}"#.utf8), exitStatus: 1) == .failed("环境未能安全停止，更新未安装"), "prepared with a failed exit is not prepared")
        try expect(CageUpdateDecision.outcome(Data(#"{"status":"error","message":"后台尚未退出"}"#.utf8), exitStatus: 1) == .failed("后台尚未退出"), "error")
        try expect(CageUpdateDecision.outcome(Data("not json".utf8), exitStatus: 0) == .failed("无法确认环境已停止"), "garbage")
        try expect(CageUpdateDecision.commitAttempt(CageUpdateDecision.commitLine(attempt: "0123456789abcdef")) == "0123456789abcdef", "commit carries attempt")
        try expect(CageUpdateDecision.commitAttempt(CageUpdateDecision.commitLine(attempt: "")) == "", "plain commit")
        for refused in ["COMMIT", "COMMIT x\n", "COMMIT 0123456789abcdef0\n", "ABORT\n", ""] {
            try expect(CageUpdateDecision.commitAttempt(Data(refused.utf8)) == nil, "commit refused: \(refused)")
        }
        try expect(CageUpdateDecision.silentMode(Data(#"{"mode":"silent"}"#.utf8)), "silent mode")
        for manual in [#"{"mode":"manual"}"#, "{}", "garbage"] { try expect(!CageUpdateDecision.silentMode(Data(manual.utf8)), "manual: \(manual)") }
        try expect(!CageUpdateDecision.silentMode(nil), "missing policy is manual")
        let decidedNow = ISO8601DateFormatter().string(from: Date())
        let release = "0123456789abcdef0123456789abcdef"
        let request = { (id: String, version: String, decided: String, release: String) in Data(#"{"id":"\#(id)","version":"\#(version)","decided_at":"\#(decided)","release":"\#(release)"}"#.utf8) }
        let accepted = CageUpdateDecision.silentRequest(request("0123456789abcdef", "0.1.16", decidedNow, release), current: "0.1.15")
        try expect(accepted?.id == "0123456789abcdef" && accepted?.release == release, "silent request")
        try expect(CageUpdateDecision.silentRequest(Data(#"{"id":"0123456789abcdef","version":"0.1.16","decided_at":"2026-11-21T03:00:00.123456789Z","release":"0123456789abcdef0123456789abcdef"}"#.utf8), current: "0.1.15", now: ISO8601DateFormatter().date(from: "2026-11-21T03:30:00Z")!) != nil, "fractional seconds from Go")
        let hourAgo = ISO8601DateFormatter().string(from: Date().addingTimeInterval(-CageUpdateDecision.requestLifetime - 1))
        let future = ISO8601DateFormatter().string(from: Date().addingTimeInterval(600))
        for refused in [request("0123456789abcdef", "0.1.15", decidedNow, release), request("0123456789abcdef", "0.1.14", decidedNow, release), request("0123456789abcdef", "0.1.16-rc.1", decidedNow, release),
                        request("short", "0.1.16", decidedNow, release), request("0123456789abcdef", "0.1.16", hourAgo, release), request("0123456789abcdef", "0.1.16", future, release),
                        request("0123456789abcdef", "0.1.16", decidedNow, "../x"), Data(#"{"id":"0123456789abcdef","version":"0.1.16","release":"0123456789abcdef0123456789abcdef"}"#.utf8)] {
            try expect(CageUpdateDecision.silentRequest(refused, current: "0.1.15") == nil, "silent request refused: \(String(decoding: refused, as: UTF8.self))")
        }
        // prepare-update's lines: the attempt is known from the moment the
        // environment stopped, so a failure or a kill after it is an abandonment.
        let stopped = #"{"status":"stopped","attempt":"0123456789abcdef"}"#
        try expect(CageUpdateDecision.outcome(Data((stopped + "\n" + #"{"status":"prepared","attempt":"0123456789abcdef"}"# + "\n").utf8), exitStatus: 0) == .prepared(attempt: "0123456789abcdef"), "stopped then prepared")
        try expect(CageUpdateDecision.outcome(Data((stopped + "\n" + #"{"status":"abandoned","attempt":"0123456789abcdef","message":"后台尚未退出"}"# + "\n").utf8), exitStatus: 1) == .abandoned(attempt: "0123456789abcdef", message: "后台尚未退出"), "abandoned after the stop")
        try expect(CageUpdateDecision.outcome(Data((stopped + "\n").utf8), exitStatus: 15) == .abandoned(attempt: "0123456789abcdef", message: "环境已停止，但升级没有继续"), "killed after the stop")
        try expect(CageUpdateDecision.outcome(Data((stopped + "\n" + #"{"status":"error","message":"x"}"# + "\n").utf8), exitStatus: 1) == .abandoned(attempt: "0123456789abcdef", message: "x"), "error after the stop")
        try expect(CageUpdateDecision.outcome(Data(#"{"status":"stopped","attempt":"bad"}"#.utf8), exitStatus: 15) == .failed("环境未能安全停止，更新未安装"), "invalid attempt ignored")
        // The outcome report, the background relaunch, a click's takeover.
        let report = try JSONSerialization.jsonObject(with: CageUpdateDecision.silentOutcome(request: "0123456789abcdef", version: "0.1.16", release: release, stage: "verify", code: "verification", message: String(repeating: "长", count: 500))) as? [String: String]
        try expect(report?["request"] == "0123456789abcdef" && report?["stage"] == "verify" && report?["code"] == "verification" && report?["message"]?.count == 200, "silent outcome report")
        try expect(CageUpdateDecision.launchesInBackground([CageUpdateDecision.backgroundKey: "1"]) && !CageUpdateDecision.launchesInBackground([:]) && !CageUpdateDecision.launchesInBackground([CageUpdateDecision.backgroundKey: "true"]), "background relaunch")
        for phase in ["checking", "downloading", "downloaded", "verifying"] { try expect(CageUpdateDecision.clickTakesOver(phase: phase, installerStarted: false), "click takes over \(phase)") }
        try expect(!CageUpdateDecision.clickTakesOver(phase: "installing", installerStarted: false) && !CageUpdateDecision.clickTakesOver(phase: "verifying", installerStarted: true), "click waits once installing")
        try expect(CageUpdateDecision.dropsPrepared(version: "0.1.16", for: "0.1.17") && !CageUpdateDecision.dropsPrepared(version: "0.1.17", for: "0.1.17"), "a package of another version is dropped for a silent request")
        // N3: the watcher acts only on the workbench's yes; anything else,
        // an older cage or noise, is no.
        try expect(CageUpdateDecision.silentAvailable(Data(#"{"available":true,"reason":""}"#.utf8)), "silent available")
        try expect(CageUpdateDecision.silentAvailable(Data(("diagnostics line\n" + #"{"available":true,"reason":""}"# + "\n").utf8)), "answer after noise")
        for refused in [#"{"available":false,"reason":"静默更新还没有在此平台的真机上验证，暂不可用；可以点击升级"}"#, "", "unknown command: silent-available", #"{"available":"true"}"#, #"{"reason":""}"#] {
            try expect(!CageUpdateDecision.silentAvailable(Data(refused.utf8)), "silent not available: \(refused)")
        }
        try expect(!CageUpdateDecision.silentAvailable(nil), "no answer is no")
        // N4: the text of a background notice is data, never script.
        let notice = CageUpdateDecision.notificationArguments(title: "Super Cage 更新未完成", message: #""; do shell script "x" --"# + String(repeating: "长", count: 400))
        try expect(notice.count == 8 && notice[6] == "Super Cage 更新未完成" && notice[7].hasPrefix(#""; do shell script"#) && notice[7].count == 300 && !notice[..<6].joined().contains("do shell script"), "notification arguments")
        // N9: the result goes to a second place when the first refuses, and
        // the newer of the two is read.
        let results = FileManager.default.temporaryDirectory.appendingPathComponent("cage-results-\(UUID().uuidString)", isDirectory: true)
        let updatesFolder = results.appendingPathComponent("updates", isDirectory: true)
        try FileManager.default.createDirectory(at: updatesFolder, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: results) }
        let primary = updatesFolder.appendingPathComponent("last-result.json")
        try expect(CageUpdateFiles.resultFallback(for: primary) == results.appendingPathComponent("app-update-result.json"), "fallback beside the updates folder")
        let refusing: (Data, URL) throws -> Void = { data, url in
            if url == primary { throw CageUpdateError("disk") }
            try CageUpdateFiles.write(data, to: url)
        }
        try expect(CageUpdateFiles.writeResult(Data(#"{"attempt":"b"}"#.utf8), primary: primary, write: refusing) == CageUpdateFiles.resultFallback(for: primary), "written to the fallback")
        try expect(CageUpdateFiles.writeResult(Data("{}".utf8), primary: primary, write: { _, _ in throw CageUpdateError("disk") }) == nil, "nowhere")
        try CageUpdateFiles.write(Data(#"{"attempt":"a"}"#.utf8), to: primary)
        try FileManager.default.setAttributes([.modificationDate: Date().addingTimeInterval(-60)], ofItemAtPath: primary.path)
        try expect(CageUpdateFiles.readResult(primary: primary) == Data(#"{"attempt":"b"}"#.utf8), "newer fallback read")
        try CageUpdateFiles.write(Data(#"{"attempt":"c"}"#.utf8), to: primary)
        try FileManager.default.setAttributes([.modificationDate: Date().addingTimeInterval(60)], ofItemAtPath: primary.path)
        try expect(CageUpdateFiles.readResult(primary: primary) == Data(#"{"attempt":"c"}"#.utf8), "newer primary read")
        // Deadlines count awake time only: a sleep does not use them up.
        var uptime: TimeInterval = 100
        let awake = CageUptimeDeadline(after: 45) { uptime }
        uptime = 144.9
        try expect(!awake.expired, "deadline before its time")
        uptime = 145
        try expect(awake.expired, "deadline at its time")

        // A silent request starts the ordinary download only while the policy
        // file says silent; a manual policy ignores the same request.
        let silentBundle = root.appendingPathComponent("silent.app")
        let silentResources = silentBundle.appendingPathComponent("Contents/Resources")
        try CageUpdateFiles.privateDirectory(silentResources)
        let silentCount = root.appendingPathComponent("silent-invocations")
        // The fake cage answers the gate question from a file the test sets,
        // and counts every other call.
        let silentGate = root.appendingPathComponent("silent-gate")
        try Data(#"{"available":false,"reason":"静默更新还没有在此平台的真机上验证，暂不可用；可以点击升级"}"#.utf8).write(to: silentGate)
        let silentScript = "#!/bin/sh\nif [ \"$1 $2\" = 'app-update silent-available' ]; then cat '\(silentGate.path)'; exit 0; fi\nprintf '%s;' \"$*\" >> '\(silentCount.path)'\nprintf '%s\\n' '{\"phase\":\"failed\",\"message\":\"offline\",\"retryable\":true,\"code\":\"transient\"}'\nexit 1\n"
        try Data(silentScript.utf8).write(to: silentResources.appendingPathComponent("cage"))
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: silentResources.appendingPathComponent("cage").path)
        let silentInfo: [String: Any] = ["CFBundleIdentifier": cageUpdateBundleID + ".silent-test", "CFBundlePackageType": "APPL", "CageVersion": "0.1.15"]
        try PropertyListSerialization.data(fromPropertyList: silentInfo, format: .xml, options: 0).write(to: silentBundle.appendingPathComponent("Contents/Info.plist"))
        let silentProfile = root.appendingPathComponent("silent-profile")
        let savedConfig = ProcessInfo.processInfo.environment["CLAUDE_CAGE_CONFIG"]
        setenv("CLAUDE_CAGE_CONFIG", silentProfile.appendingPathComponent("config.json").path, 1)
        defer { if let savedConfig { setenv("CLAUDE_CAGE_CONFIG", savedConfig, 1) } else { unsetenv("CLAUDE_CAGE_CONFIG") } }
        let updates = silentProfile.appendingPathComponent("updates")
        try CageUpdateFiles.privateDirectory(updates)
        try CageUpdateFiles.write(request("0123456789abcdef", "0.1.16", ISO8601DateFormatter().string(from: Date()), release), to: updates.appendingPathComponent("silent-request.json"))
        try CageUpdateFiles.write(Data(#"{"mode":"manual"}"#.utf8), to: updates.appendingPathComponent("policy.json"))
        var silentState: [String: Any] = [:]
        let silent = CageAppUpdater(bundleURL: silentBundle, onChange: { silentState = $0 }, quit: {}, reconnect: {})
        silent.watchSilentRequests()
        _ = RunLoop.current.run(mode: .default, before: Date().addingTimeInterval(0.3))
        try expect(!FileManager.default.fileExists(atPath: silentCount.path), "manual policy started an unattended download")
        // N3: with the gate closed, a request under a silent policy starts
        // nothing and is removed.
        try CageUpdateFiles.write(Data(#"{"mode":"silent"}"#.utf8), to: updates.appendingPathComponent("policy.json"))
        let gateDeadline = Date().addingTimeInterval(5)
        while FileManager.default.fileExists(atPath: updates.appendingPathComponent("silent-request.json").path) && Date() < gateDeadline { _ = RunLoop.current.run(mode: .default, before: Date().addingTimeInterval(0.01)) }
        _ = RunLoop.current.run(mode: .default, before: Date().addingTimeInterval(0.2))
        try expect(!FileManager.default.fileExists(atPath: updates.appendingPathComponent("silent-request.json").path) && !FileManager.default.fileExists(atPath: silentCount.path) && silentState["phase"] as? String != "downloading",
                   "a request with the gate closed started something or survived")
        // With the workbench's yes, the same request starts the download.
        try Data(#"{"available":true,"reason":""}"#.utf8).write(to: silentGate)
        try CageUpdateFiles.write(request("0123456789abcdef", "0.1.16", ISO8601DateFormatter().string(from: Date()), release), to: updates.appendingPathComponent("silent-request.json"))
        let silentDeadline = Date().addingTimeInterval(5)
        while silentState["phase"] as? String != "failed" && Date() < silentDeadline { _ = RunLoop.current.run(mode: .default, before: Date().addingTimeInterval(0.01)) }
        _ = RunLoop.current.run(mode: .default, before: Date().addingTimeInterval(0.1))
        try expect((try? String(contentsOf: silentCount, encoding: .utf8)) == "app-update download --release \(release);", "silent request started exactly one download of the judged files")
        let outcome = (try? Data(contentsOf: updates.appendingPathComponent("silent-outcome.json"))).flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: String] }
        try expect(outcome?["request"] == "0123456789abcdef" && outcome?["stage"] == "download" && outcome?["code"] == "transient" && outcome?["release"] == release, "failed unattended download reported: \(String(describing: outcome))")
        print("PASS: workbench decision arguments and answers, attempt hand-off, silent policy and request files")

        if CommandLine.arguments.count == 2 {
            let signed = URL(fileURLWithPath: CommandLine.arguments[1])
            let team = try CageUpdateFiles.signingInfo(signed, bundle: true)
            let version = Bundle(url: signed)?.object(forInfoDictionaryKey: "CageVersion") as? String ?? ""
            try CageUpdateFiles.verifyApp(signed, version: version, team: team, gatekeeper: false)
            try rejects("different publisher refused") { _ = try CageUpdateFiles.signingInfo(signed, bundle: true, expectedTeam: "AAAAAAAAAA") }
            let tampered = root.appendingPathComponent("tampered.app")
            try FileManager.default.copyItem(at: signed, to: tampered)
            let plist = tampered.appendingPathComponent("Contents/Info.plist")
            try Data("tampered".utf8).write(to: plist)
            try rejects("real signature tampering refused") { _ = try CageUpdateFiles.signingInfo(tampered, bundle: true, expectedTeam: team) }
            print("PASS: real App architecture and Developer ID signature, wrong publisher, and tampered App checks")
        }
        print("PASS: absolute profile inheritance, personal data preservation, versions, containment, replacement/double-failure rollback, process cancellation, and temporary image cleanup")
    }
}
