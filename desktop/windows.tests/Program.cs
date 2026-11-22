using Cage.Windows;

if (args is ["smoke-wait", var smokeRelease])
{
    using var deadline = new CancellationTokenSource(TimeSpan.FromSeconds(10));
    while (!File.Exists(smokeRelease)) await Task.Delay(20, deadline.Token);
    return;
}

if (args is ["app-update", "check" or "download"])
{
    File.AppendAllText(Path.Combine(Backend.Root, "retry-invocations"), "x");
    Console.WriteLine(System.Text.Json.JsonSerializer.Serialize(new { phase = "failed", message = "limited", retryable = true, retryAt = Environment.GetEnvironmentVariable("CAGE_UPDATE_RETRY_AT") }));
    Environment.Exit(1);
}

// The recovery integration tests launch a copy of this executable through the
// same subprocess/JSON boundary used by the shipped updater.
if (args is ["prepare-update", "--json", "--stop"])
{
    File.WriteAllText(Path.Combine(AppContext.BaseDirectory, "controller-invoked"), Environment.ProcessId.ToString());
    File.WriteAllText(Path.Combine(AppContext.BaseDirectory, "profile-invoked"), Environment.GetEnvironmentVariable("CLAUDE_CAGE_CONFIG") ?? "missing");
    if (File.Exists(Path.Combine(AppContext.BaseDirectory, "broken-backend")))
    {
        Console.WriteLine("{\"status\":\"failed\"}"); Environment.Exit(7);
    }
    Console.WriteLine(File.Exists(Path.Combine(AppContext.BaseDirectory, "running-environment"))
        ? "{\"status\":\"stop_required\"}" : "{\"status\":\"prepared\"}");
    return;
}

#if WINDOWS
NativeInputTests.Run();
#endif

var assertions = 0;
void Check(bool condition, string message) { assertions++; if (!condition) throw new Exception(message); }
void Reject(Action action, string message)
{
    assertions++;
    try { action(); } catch (InvalidDataException) { return; }
    throw new Exception(message);
}
foreach (var (work, dpi) in new[] {
    (new System.Drawing.Rectangle(0, 0, 3840, 2080), 96),
    (new System.Drawing.Rectangle(0, 0, 3840, 2080), 192),
    (new System.Drawing.Rectangle(0, 0, 1920, 1040), 144),
    (new System.Drawing.Rectangle(0, 0, 1366, 728), 96),
    (new System.Drawing.Rectangle(-1920, 0, 1920, 1040), 96),
    (new System.Drawing.Rectangle(0, 0, 1280, 680), 192),
})
{
    var bounds = WindowLayout.Bounds(work, dpi, null);
    Check(work.Contains(bounds), "initial window extends beyond the monitor work area");
    Check(bounds.Width >= WindowLayout.Minimum(work, dpi).Width, "initial window is below its DPI-aware minimum");
    var restored = WindowLayout.Bounds(work, dpi, new WindowPlacement(5000, 3000, 5000, 4000, 96, false));
    Check(work.Contains(restored), "saved window is inaccessible after a monitor or DPI change");
}
Check(WindowLayout.Bounds(new(0, 0, 3840, 2080), 96, null).Width > 2500, "4K window stayed at a fixed 1280 pixels");
Check(WindowLayout.Bounds(new(0, 0, 3840, 2080), 192, new(100, 100, 1000, 700, 96, false)).Width == 2000,
    "restoring a user size did not account for the new monitor DPI");
Console.WriteLine($"Windows window layout: {assertions} assertions passed.");

// Claude Science host rendering: ScienceFrame.cs against the contract the Go
// tests drive headless Chromium with (internal/workbench/testdata/science-cdp.json).
assertions = 0;
{
    using var goldenStream = typeof(Program).Assembly.GetManifestResourceStream("Cage.ScienceCDP.json")!;
    var golden = System.Text.Json.Nodes.JsonNode.Parse(goldenStream)!.AsObject();
    bool Same(System.Text.Json.Nodes.JsonNode? a, System.Text.Json.Nodes.JsonNode? b) => System.Text.Json.Nodes.JsonNode.DeepEquals(a, b);
    System.Text.Json.Nodes.JsonArray Calls(IEnumerable<ScienceFrame.Call> calls) =>
        new(calls.Select(call => (System.Text.Json.Nodes.JsonNode)new System.Text.Json.Nodes.JsonObject { ["method"] = call.Method, ["params"] = call.Params.DeepClone() }).ToArray());
    var scienceRoot = Path.Combine(Path.GetTempPath(), "cage-science-" + Guid.NewGuid().ToString("N"));
    try
    {
        Directory.CreateDirectory(Path.Combine(scienceRoot, "vms", "lab"));
        Check(ScienceFrame.Arguments(scienceRoot) == null, "switches without any profile");
        File.WriteAllText(Path.Combine(scienceRoot, "workbench.json"), "{\n  \"science_host_render\": false\n}");
        File.WriteAllText(Path.Combine(scienceRoot, "vms", "lab", "workbench.json"), "{ broken");
        Check(ScienceFrame.Arguments(scienceRoot) == null, "switches while host rendering is off");
        File.WriteAllText(Path.Combine(scienceRoot, "vms", "lab", "workbench.json"), "{\n  \"keyboard\": \"pc\",\n  \"science_host_render\": true\n}");
        Check(ScienceFrame.Arguments(scienceRoot) == ScienceFrame.BackstopArguments, "no switches with host rendering on in a named environment");
        Check(ScienceFrame.BackstopArguments == string.Join(' ', golden["backstop"]!.AsArray().Select(item => item!.GetValue<string>())),
            "the window's switches differ from the ones the browser tests prove");

        var control = golden["control"]!.DeepClone().AsObject();
        control["pid"] = Environment.ProcessId; // the golden record names no process
        var origin = "http://0123456789abcdef0123456789abcdef.localhost:" + control["science_port"];
        var entry = origin + "/_cage/enter";
        Check(ScienceFrame.LabelOrigin(entry) == origin, "label origin of an entry address");
        Check(ScienceFrame.Identity(scienceRoot, entry) == null, "identity without a control file");
        File.WriteAllText(Path.Combine(scienceRoot, "control.json"), "{\"port\":17776,\"pid\":1}");
        File.WriteAllText(Path.Combine(scienceRoot, "vms", "lab", "control.json"), control.ToJsonString());
        var identity = ScienceFrame.Identity(scienceRoot, entry);
        Check(identity != null && Same(identity, control["science_identity"]), "identity differs from the control file");
        foreach (var other in new[] {
            entry.Replace(":17840", ":17841"), entry.Replace("0123456789abcdef0123456789abcdef.localhost", "localhost"),
            entry.Replace("0123456789abcdef0123456789abcdef", "0123456789abcdef0123456789abcde"), entry.Replace("0123456789abcdef0123456789abcdef", "0123456789ABCDEFX123456789abcdef"),
            entry.Replace(".localhost", ".localhost.evil"), entry.Replace("http://", "https://"), entry.Replace("http://", "http://user@"),
            entry.Replace("/_cage/enter", "/"), entry + "?x=1", entry + "#ticket", "http://127.0.0.1:17840/_cage/enter",
            "http://0123456789abcdef0123456789abcdef.localhost/_cage/enter", "not a url",
        })
            Check(ScienceFrame.Identity(scienceRoot, other) == null, "identity for a foreign address: " + other);
        // A record left by an exited workload with the same port is skipped.
        var stale = control.DeepClone().AsObject();
        stale["pid"] = int.MaxValue;
        stale["science_identity"]!["timezone"] = "Europe/Paris";
        File.WriteAllText(Path.Combine(scienceRoot, "control.json"), stale.ToJsonString());
        Check(Same(ScienceFrame.Identity(scienceRoot, entry), control["science_identity"]), "a stale control file shadowed the running workload");
        File.WriteAllText(Path.Combine(scienceRoot, "vms", "lab", "control.json"), stale.ToJsonString());
        Check(ScienceFrame.Identity(scienceRoot, entry) == null, "identity from an exited workload");
        File.WriteAllText(Path.Combine(scienceRoot, "control.json"), "{\"port\":17776,\"pid\":1}");
        File.WriteAllText(Path.Combine(scienceRoot, "vms", "lab", "control.json"), control.ToJsonString());
        var unsafeIdentity = control.DeepClone().AsObject();
        unsafeIdentity["science_identity"]!["platform"] = "Linux\"; alert(1); \"";
        File.WriteAllText(Path.Combine(scienceRoot, "vms", "lab", "control.json"), unsafeIdentity.ToJsonString());
        Check(ScienceFrame.Identity(scienceRoot, entry) == null, "identity with a value that is not plain text");
        File.WriteAllText(Path.Combine(scienceRoot, "vms", "lab", "control.json"), control.ToJsonString());

        // Ending a frame's session: the running workload's science_end for the front's port, the label from the origin.
        var endContract = golden["end"]!.AsObject();
        Check(ScienceFrame.EndPath == endContract["path"]!.GetValue<string>() && ScienceFrame.EndSecretHeader == endContract["secret_header"]!.GetValue<string>() &&
            ScienceFrame.EndLabelHeader == endContract["label_header"]!.GetValue<string>() && ScienceFrame.EndedHeader == endContract["ended_header"]!.GetValue<string>(), "the end differs from the front's");
        Check(ScienceFrame.End(scienceRoot, origin)?.Secret == "SECRET", "the end's secret is not the golden control file's science_end");
        var ending = control.DeepClone().AsObject();
        ending.Remove("science_end");
        File.WriteAllText(Path.Combine(scienceRoot, "vms", "lab", "control.json"), ending.ToJsonString());
        Check(ScienceFrame.End(scienceRoot, origin) == null, "an end without science_end");
        ending["science_end"] = "0123456789abcdefABCDEF_-";
        File.WriteAllText(Path.Combine(scienceRoot, "vms", "lab", "control.json"), ending.ToJsonString());
        var end = ScienceFrame.End(scienceRoot, origin);
        Check(end != null && end.Address.ToString() == "http://127.0.0.1:" + control["science_port"] + "/_cage/end" &&
            end.Label == "0123456789abcdef0123456789abcdef" && end.Secret == "0123456789abcdefABCDEF_-", "end of a frame's session");
        foreach (var other in new[] { origin.Replace(":17840", ":17841"), "http://localhost:17840", origin.Replace("http://", "https://"), "", "not a url" })
            Check(ScienceFrame.End(scienceRoot, other) == null, "an end for a foreign origin: " + other);
        File.WriteAllText(Path.Combine(scienceRoot, "vms", "lab", "control.json"), control.ToJsonString());

        // The journal of framed origins: nothing at all without one (the window's
        // start reads one path), recorded once, forgotten once cleared.
        var journalData = Path.Combine(scienceRoot, "webview2-journal");
        Check(ScienceFrame.Journal(journalData).Count == 0 && !Directory.Exists(journalData), "a missing journal read or created something");
        Check(ScienceFrame.Forget(journalData, [origin]) && !Directory.Exists(journalData), "forgetting without a journal wrote something");
        Check(ScienceFrame.Record(journalData, origin, "http://localhost") && ScienceFrame.Record(journalData, origin, "http://localhost") &&
            ScienceFrame.Journal(journalData).SequenceEqual([new ScienceFrame.Framed(origin, "http://localhost")]), "journal after recording one origin twice");
        var second = origin.Replace("0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210");
        Check(ScienceFrame.Record(journalData, second, "http://localhost") && ScienceFrame.Forget(journalData, [origin]) &&
            ScienceFrame.Journal(journalData).SequenceEqual([new ScienceFrame.Framed(second, "http://localhost")]), "journal after forgetting one of two");
        Check(ScienceFrame.Forget(journalData, [second]) && !File.Exists(ScienceFrame.JournalPath(journalData)), "an empty journal stays on disk");
        File.WriteAllText(ScienceFrame.JournalPath(journalData), "{\"framed\":[{\"origin\":\"http://localhost:1\",\"site\":\"x\"},{\"origin\":\"" + origin + "\",\"site\":\"http://localhost\"}]}");
        Check(ScienceFrame.Journal(journalData).SequenceEqual([new ScienceFrame.Framed(origin, "http://localhost")]), "a journal entry that is no session origin was kept");
        File.Delete(ScienceFrame.JournalPath(journalData));

        // The calls and the policy are the ones the Go tests prove in headless Chromium.
        Check(golden["contract"]!.GetValue<int>() == ScienceFrame.Contract, "bridge contract differs");
        Check(golden["settle_ms"]!.GetValue<int>() == ScienceFrame.SettleMilliseconds, "settle time differs");
        Check(golden["navigate_wait_ms"]!.GetValue<int>() == ScienceFrame.NavigateWaitMilliseconds, "navigation wait differs");
        Check(golden["end_wait_ms"]!.GetValue<int>() == ScienceFrame.EndWaitMilliseconds, "end wait differs");
        Check(golden["crash_wait_ms"]!.GetValue<int>() == ScienceFrame.CrashWaitMilliseconds, "crash wait differs");
        Check(Same(Calls(ScienceFrame.PageAttach), golden["page_attach"]), "page attach calls differ from the contract");
        Check(Same(Calls(ScienceFrame.PageDetach), golden["page_detach"]), "page detach calls differ from the contract");
        Check(Same(Calls(ScienceFrame.Frame(identity!)), golden["frame"]), "frame calls differ from the contract");
        Check(Same(Calls(ScienceFrame.Worker(identity!)), golden["worker"]), "worker calls differ from the contract");
        Check(Same(Calls(ScienceFrame.Resume), golden["resume"]), "resume differs from the contract");
        Check(Same(Calls(ScienceFrame.ClearOrigin("ORIGIN", "SITE")), golden["clear_origin"]), "origin clearing differs from the contract");
        Check(ScienceFrame.Benign("Another locale override is already in effect") && !ScienceFrame.Benign("Target closed"), "benign locale error");
        var proxy = golden["proxy_check"]!.AsObject();
        Check(ScienceFrame.ProxyCheckURL("NONCE") == proxy["url"]!.GetValue<string>(), "proxy check address differs");
        Check(ScienceFrame.ProxyLoopbackURL("NONCE") == proxy["loopback_url"]!.GetValue<string>(), "proxy check loopback address differs");
        Check(Same(ScienceFrame.ProxyCheckOptions(), proxy["options"]), "proxy check options differ");
        Check(ScienceFrame.ProxyVerified == proxy["verified"]!.GetValue<string>(), "proxy check error differs");
        Check(ScienceFrame.ProxyVerdict(false, ScienceFrame.ProxyVerified) == "verified" && ScienceFrame.ProxyVerdict(false, "net::ERR_NAME_NOT_RESOLVED") == "failed" &&
            ScienceFrame.ProxyVerdict(true, "") == "failed" && ScienceFrame.ProxyVerdict(false, "net::ERR_HTTP_RESPONSE_CODE_FAILURE") == "failed", "proxy verdicts");
        var scenarios = 0;
        foreach (var scenario in golden["scenarios"]!.AsArray())
        {
            var policy = new ScienceFrame.Policy();
            var steps = scenario!["steps"]!.AsArray();
            for (var i = 0; i < steps.Count; i++)
            {
                var step = steps[i]!.AsObject();
                string Field(string name) => step[name]?.GetValue<string>() ?? "";
                var actions = Field("do") switch
                {
                    "arm" => policy.Arm(identity!),
                    "detach" => policy.Detach(),
                    "attached" => policy.AttachedTarget(Field("parent"), Field("session"), Field("type"), "T" + Field("session")),
                    "listed" => policy.Listed((step["targets"]?.AsArray() ?? []).Select(item => item!.GetValue<string>()).ToList(),
                        (step["known"]?.AsArray() ?? []).Select(item => item!.GetValue<string>()).ToList()),
                    "failed" => policy.CallFailed(Field("session")),
                    "settle" => policy.Settle(Field("session")),
                    "detached" => policy.DetachedTarget(Field("session")),
                    "navigate" => policy.Navigate(),
                    "navigate_timeout" => policy.NavigateTimedOut(),
                    var other => throw new InvalidDataException("unknown scenario step " + other),
                };
                var got = actions.Select(action => action.End ? "end" : action.EndFront ? "endfront" : action.Crash ? action.Session + " crash" :
                    action.Remove ? "remove" : action.Proceed ? "proceed" : (action.Session.Length == 0 ? "page" : action.Session) + " " + action.Call!.Method).ToArray();
                var want = step["expect"]!.AsArray().Select(item => item!.GetValue<string>()).ToArray();
                Check(got.SequenceEqual(want), scenario["name"] + ", step " + i + ": " + string.Join(", ", got));
            }
            scenarios++;
        }
        Check(scenarios == 18, "scenario count " + scenarios);

        // Preferences: written before the browser starts, the default again without the switches.
        var data = Path.Combine(scienceRoot, "webview2");
        var preferences = ScienceFrame.PreferencesPath(data);
        Check(ScienceFrame.SeedPreferences(data, false) && !File.Exists(preferences), "preferences written while off");
        Directory.CreateDirectory(Path.GetDirectoryName(preferences)!);
        File.WriteAllText(preferences, "{\"browser\":{\"x\":1},\"net\":{\"other\":true}}");
        Check(ScienceFrame.SeedPreferences(data, true), "preferences not written");
        var seeded = System.Text.Json.Nodes.JsonNode.Parse(File.ReadAllText(preferences))!;
        Check(Same(seeded["net"]!["network_prediction_options"], golden["preferences"]!["net"]!["network_prediction_options"]) &&
            seeded["browser"]!["x"]!.GetValue<int>() == 1 && seeded["net"]!["other"]!.GetValue<bool>(), "preferences differ from the contract or lost a value");
        Check(ScienceFrame.SeedPreferences(data, false), "preferences not restored");
        var restored = System.Text.Json.Nodes.JsonNode.Parse(File.ReadAllText(preferences))!;
        Check(restored["net"]!["network_prediction_options"] == null && restored["net"]!["other"]!.GetValue<bool>(), "prediction stayed off without the switches");
        // Off, with nothing of ours in the file: the file is not rewritten.
        var untouched = "{\"net\": {\"other\": true},  \"browser\": {}}";
        File.WriteAllText(preferences, untouched);
        var written = File.GetLastWriteTimeUtc(preferences);
        Check(ScienceFrame.SeedPreferences(data, false) && File.ReadAllText(preferences) == untouched && File.GetLastWriteTimeUtc(preferences) == written, "preferences rewritten while off");
        File.WriteAllText(preferences, "{ broken");
        Check(!ScienceFrame.SeedPreferences(data, true) && File.ReadAllText(preferences) == "{ broken", "an unreadable preferences file was replaced");
    }
    finally { Directory.Delete(scienceRoot, true); }
}
Console.WriteLine($"Claude Science frame: {assertions} assertions passed.");

assertions = 0;
Check(UpdatePolicy.IsWorkbench("http://localhost:4321/", 4321), "expected local origin rejected");
foreach (var origin in new[] { "https://localhost:4321/", "http://localhost:4322/", "http://localhost.evil:4321/", "http://user@localhost:4321/", "http://127.0.0.1:4321/", "file:///C:/workbench.html" })
    Check(!UpdatePolicy.IsWorkbench(origin, 4321), "foreign native bridge origin accepted: " + origin);
Check(UpdatePolicy.Entry("http://localhost:4321/#token").Port == 4321, "entry URL rejected");
Reject(() => UpdatePolicy.Entry("http://localhost:4321/"), "entry without token accepted");
Check(UpdatePolicy.IsNewer("0.1.0-alpha.10", "0.1.0-alpha.9"), "numeric prerelease ordering");
Check(UpdatePolicy.IsNewer("0.1.0", "0.1.0-alpha.10"), "stable release ordering");
Check(UpdatePolicy.IsNewer("1.0.0-alpha.beta", "1.0.0-alpha.10"), "text prerelease ordering");
Check(!UpdatePolicy.IsNewer("0.1.0-alpha.01", "0.1.0-alpha.0"), "leading zero accepted");
Check(!UpdatePolicy.IsNewer("0.1.0-alpha.1", "0.1.0-alpha.1"), "same version accepted");
Check(!UpdatePolicy.IsNewer("0.1.0-alpha.1", "0.1.0"), "downgrade accepted");
Check(!UpdatePolicy.IsNewer("v0.2.0", "0.1.0"), "noncanonical version accepted");
Check(!UpdatePolicy.IsNewer("0.2.0+metadata", "0.1.0"), "unmatched backend version accepted");
string Organization(string subject) => UpdatePolicy.PublisherOrganization(new System.Security.Cryptography.X509Certificates.X500DistinguishedName(subject));
Check(Organization("C=CN, L=Shanghai, O=Example Publisher, CN=Example Publisher") == Organization("CN=Renewed name, O=Example Publisher, L=Beijing, C=CN"),
    "renewed certificate of the same organization rejected");
Check(Organization("C=CN, O=Example Publisher") != Organization("C=US, O=Example Publisher"), "organization of another country accepted");
Check(Organization("C=CN, O=Example Publisher") != Organization("C=CN, O=Example Publisher Ltd"), "different organization accepted");
foreach (var subject in new[] { "CN=Example Publisher", "C=CN, CN=Example Publisher", "O=Example Publisher", "C=CN, O=First, O=Second" })
    Check(Organization(subject).Length == 0, "incomplete or ambiguous publisher accepted: " + subject);
var root = Path.GetFullPath(Path.Combine(Path.GetTempPath(), "cage-update-policy-tests"));
var good = Path.Combine(root, "download-abc", "Cage-setup.exe");
Check(UpdatePolicy.DownloadPath(good, root) == good, "private download rejected");
foreach (var path in new[] { Path.Combine(root + "-other", "download-abc", "Cage-setup.exe"), Path.Combine(root, "verified-abc", "Cage-setup.exe"), Path.Combine(root, "download-abc", "nested", "Cage-setup.exe"), Path.Combine(root, "download-abc", "Cage.exe") })
    Reject(() => UpdatePolicy.DownloadPath(path, root), "untrusted download path accepted: " + path);
Console.WriteLine($"Windows update/bridge policy: {assertions} assertions passed.");

var integration = Path.Combine(OperatingSystem.IsMacOS() ? "/private/tmp" : Path.GetTempPath(), "cage-recovery-" + Guid.NewGuid());
Directory.CreateDirectory(integration);
var savedProfile = Environment.GetEnvironmentVariable("CLAUDE_CAGE_CONFIG");
try
{
    var selectedProfile = Path.Combine(integration, "selected profile", "config.json");
    Environment.SetEnvironmentVariable("CLAUDE_CAGE_CONFIG", Path.GetRelativePath(Environment.CurrentDirectory, selectedProfile));
    var backup = Path.Combine(integration, "old application");
    var candidate = Path.Combine(integration, "new application");
    void InstallFixture(string app, bool broken)
    {
        var resources = Path.Combine(app, "resources"); Directory.CreateDirectory(resources);
        foreach (var file in Directory.EnumerateFiles(AppContext.BaseDirectory))
            File.Copy(file, Path.Combine(resources, Path.GetFileName(file)));
        var controller = Path.Combine(resources, "cage.exe");
        File.Copy(Environment.ProcessPath!, controller, true);
        if (!OperatingSystem.IsWindows()) File.SetUnixFileMode(controller, File.GetUnixFileMode(Environment.ProcessPath!));
        if (broken) File.WriteAllText(Path.Combine(resources, "broken-backend"), "cannot start");
    }
    InstallFixture(backup, false); InstallFixture(candidate, true);
    var snapshotSource = Path.Combine(integration, "snapshot source");
    var snapshotTarget = Path.Combine(integration, "snapshot target");
    Directory.CreateDirectory(Path.Combine(snapshotSource, "my-project"));
    File.WriteAllText(Path.Combine(snapshotSource, "program.txt"), "program");
    File.WriteAllText(Path.Combine(snapshotSource, "my-project", "work.txt"), "personal data");
    File.WriteAllText(Path.Combine(snapshotSource, "Cage.install-files.json"), System.Text.Json.JsonSerializer.Serialize(new {
        version = 1, files = new[] { new { path = "program.txt", sha256 = new string('a', 64) } },
    }));
    var copyTree = typeof(UpdateInstaller).GetMethod("CopyTree", System.Reflection.BindingFlags.NonPublic | System.Reflection.BindingFlags.Static)!;
    copyTree.Invoke(null, [snapshotSource, snapshotTarget]);
    Check(File.ReadAllText(Path.Combine(snapshotTarget, "program.txt")) == "program", "update snapshot missed a declared program file");
    Check(!Directory.Exists(Path.Combine(snapshotTarget, "my-project")), "update snapshot copied live user data into the program rollback snapshot");
    Check(File.ReadAllText(Path.Combine(snapshotSource, "my-project", "work.txt")) == "personal data", "snapshot changed personal data");
    var nestedSnapshot = Path.Combine(snapshotSource, "updates", "backup");
    var rejectedNestedSnapshot = false;
    try { copyTree.Invoke(null, [snapshotSource, nestedSnapshot]); }
    catch (System.Reflection.TargetInvocationException error) when (error.InnerException is InvalidDataException) { rejectedNestedSnapshot = true; }
    Check(rejectedNestedSnapshot, "snapshot recursively copied its own output");
    Check(!Directory.Exists(nestedSnapshot), "overlapping snapshot destination was created before validation");
    var intactApp = Path.Combine(integration, "intact application");
    var intactBackup = Path.Combine(integration, "intact backup");
    foreach (var directory in new[] { intactApp, intactBackup })
    {
        Directory.CreateDirectory(Path.Combine(directory, "resources"));
        File.WriteAllText(Path.Combine(directory, "Cage.exe"), "original launcher");
        File.WriteAllText(Path.Combine(directory, "Cage.install-id"), "io.github.lee-xeo.claude-cage");
        File.WriteAllText(Path.Combine(directory, "resources", "cage.exe"), "original backend");
    }
    Directory.CreateDirectory(Path.Combine(intactApp, "my-project"));
    File.WriteAllText(Path.Combine(intactApp, "my-project", "work.txt"), "latest personal data");
    Check(UpdateRecovery.OriginalIntact(intactApp, intactBackup), "unchanged original was sent through replacement rollback");
    File.WriteAllText(Path.Combine(intactApp, "resources", "cage.exe"), "modified backend");
    Check(!UpdateRecovery.OriginalIntact(intactApp, intactBackup), "same launcher hid a changed backend");
    File.Copy(Path.Combine(intactBackup, "resources", "cage.exe"), Path.Combine(intactApp, "resources", "cage.exe"), true);
    File.WriteAllText(intactApp + ".cage-installation.json", "unresolved transaction");
    Check(!UpdateRecovery.OriginalIntact(intactApp, intactBackup), "unfinished personal-file transfer was treated as intact");
    File.Delete(intactApp + ".cage-installation.json");
    File.Delete(Path.Combine(intactApp, "Cage.exe"));
    Check(!UpdateRecovery.OriginalIntact(intactApp, intactBackup), "missing launcher was treated as intact");
    Check(File.ReadAllText(Path.Combine(intactApp, "my-project", "work.txt")) == "latest personal data", "recovery classification changed personal data");
    var ensureComplete = typeof(UpdateInstaller).GetMethod("EnsureInstallationComplete", System.Reflection.BindingFlags.NonPublic | System.Reflection.BindingFlags.Static)!;
    ensureComplete.Invoke(null, [snapshotSource]);
    var recoveryReceipt = snapshotSource + ".cage-installation.json";
    File.WriteAllText(recoveryReceipt, System.Text.Json.JsonSerializer.Serialize(new { stagePath = candidate, backupPath = backup }));
    string? pendingRecovery = null;
    try { ensureComplete.Invoke(null, [snapshotSource]); }
    catch (System.Reflection.TargetInvocationException error) when (error.InnerException is IOException) { pendingRecovery = error.InnerException.Message; }
    Check(pendingRecovery != null, "updater accepted a partially restored installation");
    Check(pendingRecovery!.Contains(candidate) && pendingRecovery.Contains(backup), "recovery error omitted the preserved directories");
    Check(File.Exists(recoveryReceipt), "recovery validation removed the persistent receipt");
    File.Delete(recoveryReceipt);
    var rejectedMissingInstallation = false;
    try { ensureComplete.Invoke(null, [Path.Combine(integration, "missing installation")]); }
    catch (System.Reflection.TargetInvocationException error) when (error.InnerException is IOException) { rejectedMissingInstallation = true; }
    Check(rejectedMissingInstallation, "updater accepted a missing installation and could restore a program-only snapshot");
    if (!OperatingSystem.IsWindows())
    {
        File.Delete(Path.Combine(snapshotSource, "Cage.install-files.json"));
        Directory.CreateSymbolicLink(Path.Combine(snapshotSource, "external-link"), Path.Combine(snapshotSource, "my-project"));
        var legacySnapshot = Path.Combine(integration, "legacy snapshot");
        copyTree.Invoke(null, [snapshotSource, legacySnapshot]);
        Check(File.ReadAllText(Path.Combine(legacySnapshot, "my-project", "work.txt")) == "personal data", "legacy snapshot missed a regular file");
        Check(!Directory.Exists(Path.Combine(legacySnapshot, "external-link")), "legacy snapshot followed a user link");
    }
    await UpdateRecovery.PrepareRollback(backup);
    Check(File.Exists(Path.Combine(backup, "resources", "controller-invoked")), "recovery did not execute the old backend");
    Check(!File.Exists(Path.Combine(candidate, "resources", "controller-invoked")), "recovery executed the broken candidate backend");
    Check(File.ReadAllText(Path.Combine(backup, "resources", "profile-invoked")) == selectedProfile, "recovery child switched away from the original relative profile");
    File.WriteAllText(Path.Combine(backup, "resources", "running-environment"), "still running");
    var rejected = false;
    try { await UpdateRecovery.PrepareRollback(backup); } catch (IOException) { rejected = true; }
    Check(rejected, "recovery continued without a prepared response from the old backend");

    var garbage = Path.Combine(integration, "locked backup"); Directory.CreateDirectory(garbage);
    var lockedFile = Path.Combine(garbage, "old.dll"); File.WriteAllText(lockedFile, "old version");
    var download = Path.Combine(integration, "download"); Directory.CreateDirectory(download);
    var marker = Path.Combine(integration, "ack.json"); File.WriteAllText(marker, "confirmed");
    var liveApp = Path.Combine(candidate, "Cage.exe"); File.WriteAllText(liveApp, "healthy new application");
    FileStream? locked = null;
    if (OperatingSystem.IsWindows()) locked = new FileStream(lockedFile, FileMode.Open, FileAccess.Read, FileShare.Read);
    else File.SetUnixFileMode(garbage, UnixFileMode.UserRead | UnixFileMode.UserExecute);
    try
    {
        UpdateRecovery.CleanupAfterSuccess(garbage, download, marker);
        Check(File.ReadAllText(liveApp) == "healthy new application", "cleanup changed the confirmed new application");
        Check(File.Exists(lockedFile), "fixture did not cause the intended cleanup failure");
        Check(!Directory.Exists(download) && !File.Exists(marker), "one cleanup failure prevented independent cleanup");
    }
    finally
    {
        locked?.Dispose();
        if (!OperatingSystem.IsWindows()) File.SetUnixFileMode(garbage, UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute);
    }
    Console.WriteLine($"Windows snapshot/recovery integration: {assertions - 22} assertions passed; actual child process and filesystem failures exercised.");

    // Exercise the production updater's streamed state and process-exit path.
    var nativeResources = Path.Combine(AppContext.BaseDirectory, "resources");
    if (Directory.Exists(nativeResources)) throw new IOException("retry fixture requires an unused test resources directory");
    Directory.CreateDirectory(nativeResources);
    var savedRetry = Environment.GetEnvironmentVariable("CAGE_UPDATE_RETRY_AT");
    try
    {
        foreach (var file in Directory.EnumerateFiles(AppContext.BaseDirectory)) File.Copy(file, Path.Combine(nativeResources, Path.GetFileName(file)));
        var executable = Path.Combine(nativeResources, "cage.exe");
        File.Copy(Environment.ProcessPath!, executable, true);
        if (!OperatingSystem.IsWindows()) File.SetUnixFileMode(executable, File.GetUnixFileMode(Environment.ProcessPath!));
        var release = Path.Combine(integration, "smoke-release");
        using (var owned = System.Diagnostics.Process.Start(Backend.StartInfo(executable, "smoke-wait", release))!)
        {
            var record = Path.Combine(Backend.Root, "control.json");
            Directory.CreateDirectory(Backend.Root);
            var controllers = new List<System.Diagnostics.Process>();
            try
            {
                File.WriteAllText(record, System.Text.Json.JsonSerializer.Serialize(new { pid = Environment.ProcessId }));
                var foreignRejected = false;
                try { Backend.CaptureSmokeControllers(); } catch (IOException) { foreignRejected = true; }
                Check(foreignRejected, "smoke captured a controller outside its application executable");
                File.WriteAllText(record, System.Text.Json.JsonSerializer.Serialize(new { pid = owned.Id }));
                controllers = Backend.CaptureSmokeControllers();
                Check(controllers.Count == 1 && !controllers[0].HasExited, "smoke did not pin its live controller");
                var exit = controllers[0].WaitForExitAsync();
                Check(!exit.IsCompleted, "smoke considered a still-running controller stopped");
                File.WriteAllText(release, "release owned child");
                await exit.WaitAsync(TimeSpan.FromSeconds(5));
                Check(controllers[0].HasExited, "smoke did not observe actual controller exit");
            }
            finally
            {
                File.WriteAllText(release, "release owned child");
                if (!owned.WaitForExit(5000)) { owned.Kill(); owned.WaitForExit(); }
                foreach (var controller in controllers) controller.Dispose();
                File.Delete(record);
            }
        }
        Console.WriteLine("Windows smoke shutdown: actual controller handle pinned, foreign executable rejected, process exit observed.");
        var retryAt = DateTimeOffset.UtcNow.AddHours(1).ToString("yyyy-MM-dd'T'HH:mm:ss'Z'");
        Directory.CreateDirectory(Backend.Root);
        Environment.SetEnvironmentVariable("CAGE_UPDATE_RETRY_AT", retryAt);
        var completion = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        var failures = 0;
        using var updater = new AppUpdater(state => { if (state.GetValueOrDefault("phase") as string == "failed" && Interlocked.Increment(ref failures) == 2) completion.TrySetResult(); }, () => throw new Exception("unexpected installation"), () => {});
        updater.Handle("check");
        await completion.Task.WaitAsync(TimeSpan.FromSeconds(5));
        Check(updater.Handle("status").GetValueOrDefault("retryAt") as string == retryAt, "process-exit path lost retry deadline");
        updater.Handle("check"); updater.Handle("install");
        await Task.Delay(100);
        Check(File.ReadAllText(Path.Combine(Backend.Root, "retry-invocations")) == "x", "native check/install bypassed server cooldown");
        Console.WriteLine("Windows updater protocol: real subprocess preserved deadline; early check/install blocked.");
    }
    finally
    {
        Environment.SetEnvironmentVariable("CAGE_UPDATE_RETRY_AT", savedRetry);
        Directory.Delete(nativeResources, true);
    }
}
finally {
    Environment.SetEnvironmentVariable("CLAUDE_CAGE_CONFIG", savedProfile);
    Directory.Delete(integration, true);
}
