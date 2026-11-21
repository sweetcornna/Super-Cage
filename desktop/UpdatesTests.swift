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
            .blocked(code: "must_wait", message: "以下操作完成后才能升级：「更新环境组件」正在进行"), "blocked")
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
        try expect(CageUpdateDecision.silentRequest(Data(#"{"id":"0123456789abcdef","version":"0.1.16"}"#.utf8), current: "0.1.15")?.id == "0123456789abcdef", "silent request")
        for refused in [#"{"id":"0123456789abcdef","version":"0.1.15"}"#, #"{"id":"0123456789abcdef","version":"0.1.14"}"#, #"{"id":"0123456789abcdef","version":"0.1.16-rc.1"}"#, #"{"id":"short","version":"0.1.16"}"#] {
            try expect(CageUpdateDecision.silentRequest(Data(refused.utf8), current: "0.1.15") == nil, "silent request refused: \(refused)")
        }

        // A silent request starts the ordinary download only while the policy
        // file says silent; a manual policy ignores the same request.
        let silentBundle = root.appendingPathComponent("silent.app")
        let silentResources = silentBundle.appendingPathComponent("Contents/Resources")
        try CageUpdateFiles.privateDirectory(silentResources)
        let silentCount = root.appendingPathComponent("silent-invocations")
        let silentScript = "#!/bin/sh\nprintf '%s;' \"$*\" >> '\(silentCount.path)'\nprintf '%s\\n' '{\"phase\":\"failed\",\"message\":\"offline\",\"retryable\":true}'\nexit 1\n"
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
        try CageUpdateFiles.write(Data(#"{"id":"0123456789abcdef","version":"0.1.16","decided_at":"2026-11-21T03:00:00Z"}"#.utf8), to: updates.appendingPathComponent("silent-request.json"))
        try CageUpdateFiles.write(Data(#"{"mode":"manual"}"#.utf8), to: updates.appendingPathComponent("policy.json"))
        var silentState: [String: Any] = [:]
        let silent = CageAppUpdater(bundleURL: silentBundle, onChange: { silentState = $0 }, quit: {}, reconnect: {})
        silent.watchSilentRequests()
        _ = RunLoop.current.run(mode: .default, before: Date().addingTimeInterval(0.3))
        try expect(!FileManager.default.fileExists(atPath: silentCount.path), "manual policy started an unattended download")
        try CageUpdateFiles.write(Data(#"{"mode":"silent"}"#.utf8), to: updates.appendingPathComponent("policy.json"))
        let silentDeadline = Date().addingTimeInterval(5)
        while silentState["phase"] as? String != "failed" && Date() < silentDeadline { _ = RunLoop.current.run(mode: .default, before: Date().addingTimeInterval(0.01)) }
        _ = RunLoop.current.run(mode: .default, before: Date().addingTimeInterval(0.1))
        try expect((try? String(contentsOf: silentCount, encoding: .utf8)) == "app-update download;", "silent request started exactly one download")
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
