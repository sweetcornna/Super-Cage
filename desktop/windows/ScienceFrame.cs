using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.RegularExpressions;

namespace Cage.Windows;

// Claude Science host rendering (phase 2a), the part without WebView2: which
// switches the window needs, which front a frame address belongs to, and the
// DevTools calls that give the page the guest's identity. desktop/windows.tests
// checks these against internal/workbench/testdata/science-cdp.json, the
// contract the Go tests run in headless Chromium. ScienceFrameHost applies
// them to the WebView.
internal static partial class ScienceFrame
{
    // Only while some environment has host rendering on: no proxy (loopback
    // still bypasses it, so the workbench and the front load), no non-proxied
    // WebRTC, no shared workers (CDP cannot pause them before they run), no
    // hyperlink-auditing pings.
    public const string BackstopArguments = "--proxy-server=http://127.0.0.1:9 --webrtc-ip-handling-policy=disable_non_proxied_udp --disable-shared-workers --no-pings";

    public sealed record Call(string Method, JsonObject Params);

    // The profiles a workload can run in: the default one and each named VM.
    private static IEnumerable<string> Profiles(string root)
    {
        yield return root;
        var vms = Path.Combine(root, "vms");
        string[] names;
        try { names = Directory.Exists(vms) ? Directory.GetDirectories(vms) : []; }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException) { names = []; }
        foreach (var name in names.Order(StringComparer.Ordinal)) yield return name;
    }

    private static JsonObject? ReadObject(string path)
    {
        try { return File.Exists(path) ? JsonNode.Parse(File.ReadAllText(path)) as JsonObject : null; }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException or JsonException) { return null; }
    }

    // Arguments is null (the environment options the window always had)
    // unless some profile's workbench.json has science_host_render.
    public static string? Arguments(string root) =>
        Profiles(root).Any(profile => ReadObject(Path.Combine(profile, "workbench.json"))?["science_host_render"] is JsonValue value &&
            value.TryGetValue<bool>(out var on) && on) ? BackstopArguments : null;

    // Identity is what a running workload published in its control.json for
    // the front at the address's port; null for anything that is not a
    // front's entry. A record left by a workload that exited is skipped: the
    // port it names may belong to another environment's front now.
    public static JsonObject? Identity(string root, string raw)
    {
        if (!Uri.TryCreate(raw, UriKind.Absolute, out var url) || url.Scheme != "http" || url.Host != "localhost" ||
            url.IsDefaultPort || url.UserInfo.Length != 0 || url.AbsolutePath != "/_cage/enter" || url.Query.Length != 0 || url.Fragment.Length < 2)
            return null;
        foreach (var profile in Profiles(root))
        {
            var control = ReadObject(Path.Combine(profile, "control.json"));
            if (control?["science_port"] is not JsonValue port || !port.TryGetValue<int>(out var number) || number != url.Port) continue;
            if (control["pid"] is not JsonValue pid || !pid.TryGetValue<int>(out var process) || !Running(process)) continue;
            if (control["science_identity"] is JsonObject identity && Valid(identity)) return (JsonObject)identity.DeepClone();
        }
        return null;
    }

    private static bool Running(int pid)
    {
        if (pid <= 0) return false;
        try { using var process = System.Diagnostics.Process.GetProcessById(pid); return !process.HasExited; }
        catch (Exception error) when (error is ArgumentException or InvalidOperationException) { return false; }
    }

    [GeneratedRegex(@"^[A-Za-z0-9 _.,;:=()/+-]{1,128}$")]
    private static partial Regex Plain();

    private static string Text(JsonObject identity, string name) =>
        identity[name] is JsonValue value && value.TryGetValue<string>(out var text) ? text : "";

    private static bool Valid(JsonObject identity) =>
        new[] { "timezone", "locale", "accept_language", "platform" }.All(name => Plain().IsMatch(Text(identity, name))) &&
        Text(identity, "user_agent") is { Length: > 0 and <= 512 } agent && !agent.Any(char.IsControl) &&
        identity["ua_metadata"] is JsonObject;

    private static Call Make(string method, JsonObject parameters) => new(method, parameters);

    private static Call UserAgent(JsonObject identity) => Make("Emulation.setUserAgentOverride", new JsonObject {
        ["userAgent"] = Text(identity, "user_agent"),
        ["acceptLanguage"] = Text(identity, "accept_language"),
        ["platform"] = Text(identity, "platform"),
        ["userAgentMetadata"] = identity["ua_metadata"]!.DeepClone(),
    });

    private static Call AutoAttach(bool on) => Make("Target.setAutoAttach", new JsonObject {
        ["autoAttach"] = on, ["waitForDebuggerOnStart"] = on, ["flatten"] = true,
    });

    public static Call Resume => Make("Runtime.runIfWaitingForDebugger", new JsonObject());

    // The page (and a frame in its own process): time zone, locale, user agent,
    // then pause every new worker and frame until it has them too.
    public static Call[] Arm(JsonObject identity) => [
        Make("Emulation.setTimezoneOverride", new JsonObject { ["timezoneId"] = Text(identity, "timezone") }),
        Make("Emulation.setLocaleOverride", new JsonObject { ["locale"] = Text(identity, "locale") }),
        UserAgent(identity),
        AutoAttach(true),
    ];

    // A dedicated worker: CDP has no navigator.platform override for workers,
    // so the getter is replaced before the worker's script runs.
    public static Call[] Worker(JsonObject identity) => [
        UserAgent(identity),
        Make("Runtime.evaluate", new JsonObject {
            ["expression"] = "Object.defineProperty(WorkerNavigator.prototype, 'platform', { get() { return " +
                JsonSerializer.Serialize(Text(identity, "platform")) + "; }, configurable: true, enumerable: true }); true",
        }),
        AutoAttach(true),
        Resume,
    ];

    // A frame in another process gets the page's calls, then runs.
    public static Call[] Frame(JsonObject identity) => [.. Arm(identity), Resume];

    public static Call[] Disarm => [
        AutoAttach(false),
        Make("Emulation.setTimezoneOverride", new JsonObject { ["timezoneId"] = "" }),
        Make("Emulation.setLocaleOverride", new JsonObject()),
        Make("Emulation.setUserAgentOverride", new JsonObject { ["userAgent"] = "" }),
    ];

    // The locale override is per renderer process: a second target in the
    // same process reports the one already set.
    public static bool Benign(string message) => message.Contains("Another locale override is already in effect", StringComparison.Ordinal);

    // What a newly attached target gets: workers and frames their calls;
    // service and shared workers only resume (Emulation calls never answer
    // there); anything else resumes.
    public static Call[] ForTarget(string type, JsonObject identity) => type switch {
        "worker" => Worker(identity),
        "iframe" => Frame(identity),
        _ => [Resume],
    };
}
