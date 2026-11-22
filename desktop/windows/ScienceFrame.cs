using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.RegularExpressions;

namespace Cage.Windows;

// Claude Science host rendering (phase 2a), the part without WebView2: which
// switches the window needs, which environment a frame address belongs to,
// the DevTools calls that give the Science frame's own targets the guest's
// identity, and the policy that decides which target gets which calls.
// desktop/windows.tests checks all of it against
// internal/workbench/testdata/science-cdp.json, the contract the Go tests run
// in headless Chromium (science_shell_test.go has the same policy).
// ScienceFrameHost applies it to the WebView.
internal static partial class ScienceFrame
{
    // The scienceFrame bridge version, published to the workbench as
    // window.cageNativeScienceFrame (MainWindow.BridgeScript): 2 gives the
    // identity to the frame's targets only; the workbench keeps the host's.
    public const int Contract = 2;

    // Only while some environment has host rendering on: no proxy (loopback
    // still bypasses it, so the workbench and the front load), no non-proxied
    // WebRTC, no shared workers (CDP cannot pause them before they run), no
    // hyperlink-auditing pings.
    // The bypass list turns the implicit loopback rules off and bypasses
    // only the names and addresses the workbench and the fronts use, so a
    // loopback address outside it reaches the proxy (round-2 N3).
    public const string BackstopArguments = "--proxy-server=http://127.0.0.1:9 --proxy-bypass-list=<-loopback>;localhost;127.0.0.1;[::1];*.localhost --webrtc-ip-handling-policy=disable_non_proxied_udp --disable-shared-workers --no-pings";

    // How long a target whose call failed may take to detach before the frame
    // ends, and how long a held top-level navigation waits for the frame's
    // targets. Within that wait, the teardown waits at most EndWait for the
    // front's end and CrashWait for a crashed frame's Inspector.targetCrashed
    // (WebView2 157 took up to about 1 s) before it removes the frame; while
    // the frame goes, its targets are listed every ListEvery.
    public const int SettleMilliseconds = 2000;
    public const int NavigateWaitMilliseconds = 5000;
    public const int EndWaitMilliseconds = 2500;
    public const int CrashWaitMilliseconds = 1500;
    public const int ListEveryMilliseconds = 1000;

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

    [GeneratedRegex(@"^[0-9a-f]{32}\.localhost$")]
    private static partial Regex Label();

    // LabelOrigin is the session origin of a front's entry address,
    // http://<label>.localhost:<port>/_cage/enter (science_front.go), or null.
    public static string? LabelOrigin(string raw)
    {
        if (!Uri.TryCreate(raw, UriKind.Absolute, out var url) || url.Scheme != "http" || !Label().IsMatch(url.Host) ||
            url.IsDefaultPort || url.UserInfo.Length != 0 || url.AbsolutePath != "/_cage/enter" || url.Query.Length != 0 || url.Fragment.Length != 0)
            return null;
        return url.GetLeftPart(UriPartial.Authority);
    }

    // Identity is what a running workload published in its control.json for
    // the front at the address's port; null for anything that is not a
    // front's entry. A record left by a workload that exited is skipped: the
    // port it names may belong to another environment's front now.
    public static JsonObject? Identity(string root, string raw)
    {
        if (LabelOrigin(raw) == null) return null;
        var port = new Uri(raw).Port;
        foreach (var profile in Profiles(root))
        {
            var control = ReadObject(Path.Combine(profile, "control.json"));
            if (control?["science_port"] is not JsonValue value || !value.TryGetValue<int>(out var number) || number != port) continue;
            if (control["pid"] is not JsonValue pid || !pid.TryGetValue<int>(out var process) || !Running(process)) continue;
            if (control["science_identity"] is JsonObject identity && Valid(identity)) return (JsonObject)identity.DeepClone();
        }
        return null;
    }

    // End is where and how the shell ends the front's session for the frame
    // of a session origin (POST /_cage/end, science_front.go serveEnd): the
    // front's loopback address, the label and the running workload's
    // science_end; null when no running workload publishes that front.
    public sealed record FrontEnd(Uri Address, string Label, string Secret);

    public static FrontEnd? End(string root, string origin)
    {
        if (!Uri.TryCreate(origin, UriKind.Absolute, out var url) || url.Scheme != "http" || !Label().IsMatch(url.Host) || url.IsDefaultPort) return null;
        foreach (var profile in Profiles(root))
        {
            var control = ReadObject(Path.Combine(profile, "control.json"));
            if (control?["science_port"] is not JsonValue value || !value.TryGetValue<int>(out var number) || number != url.Port) continue;
            if (control["pid"] is not JsonValue pid || !pid.TryGetValue<int>(out var process) || !Running(process)) continue;
            var secret = Text(control, "science_end");
            if (secret.Length == 0 || !Plain().IsMatch(secret)) continue;
            return new FrontEnd(new Uri("http://127.0.0.1:" + url.Port + "/_cage/end"), url.Host[..^".localhost".Length], secret);
        }
        return null;
    }

    // RemoveFrame removes the pane's frame elements of one session origin
    // (all of them for ""), wherever the pane parked them.
    public static string RemoveFrame(string origin) =>
        "document.querySelectorAll('iframe[data-science-frame]').forEach((frame) => { try { if (" + JsonSerializer.Serialize(origin) +
        " === '' || new URL(frame.src).origin === " + JsonSerializer.Serialize(origin) + ") frame.remove(); } catch {} }); true";

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

    public static Call[] Resume => [Make("Runtime.runIfWaitingForDebugger", new JsonObject())];

    // The window's page session only watches: every new target starts paused.
    public static Call[] PageAttach => [AutoAttach(true)];

    // Turning it off drops every override and lets paused targets run: the
    // policy sends it only once no guest target is left.
    public static Call[] PageDetach => [AutoAttach(false)];

    // The Science frame (and a frame inside it): time zone, locale, user
    // agent; no host file chooser (the page sees a cancel; interception needs
    // the Page domain); its own new workers and frames paused until they have
    // them too; then it runs.
    public static Call[] Frame(JsonObject identity) => [
        Make("Emulation.setTimezoneOverride", new JsonObject { ["timezoneId"] = Text(identity, "timezone") }),
        Make("Emulation.setLocaleOverride", new JsonObject { ["locale"] = Text(identity, "locale") }),
        UserAgent(identity),
        Make("Page.enable", new JsonObject()),
        Make("Page.setInterceptFileChooserDialog", new JsonObject { ["enabled"] = true, ["cancel"] = true }),
        AutoAttach(true),
        .. Resume,
    ];

    // A dedicated worker of the frame: CDP has no navigator.platform override
    // for workers, so the getter is replaced before the worker's script runs.
    public static Call[] Worker(JsonObject identity) => [
        UserAgent(identity),
        Make("Runtime.evaluate", new JsonObject {
            ["expression"] = "Object.defineProperty(WorkerNavigator.prototype, 'platform', { get() { return " +
                JsonSerializer.Serialize(Text(identity, "platform")) + "; }, configurable: true, enumerable: true }); true",
        }),
        AutoAttach(true),
        .. Resume,
    ];

    // Once auto-attach has ended, per session origin the window framed: each
    // session has its own origin, so what it stored would never be read
    // again, only pile up. The frame's storage is partitioned under the
    // window's site (storage key "<origin>/^0<site>", Chromium's
    // serialization); the origin's own key is cleared too.
    public static Call[] ClearOrigin(string origin, string site) => [
        Make("Storage.clearDataForStorageKey", new JsonObject { ["storageKey"] = origin + "/^0" + site, ["storageTypes"] = "all" }),
        Make("Storage.clearDataForOrigin", new JsonObject { ["origin"] = origin, ["storageTypes"] = "all" }),
    ];

    // The journal of framed session origins (round-2 N6): the window writes
    // a session origin (with its own site, the storage partition) before it
    // attaches for it and forgets it once its storage is cleared. A window
    // that is killed or whose browser crashes never clears it, so the next
    // start does (ScienceFrameHost.Sweep), with or without the switches.
    // Without a journal that start reads nothing else.
    public sealed record Framed(string Origin, string Site);

    public static string JournalPath(string userDataFolder) => Path.Combine(userDataFolder, "cage-science-framed.json");

    public static List<Framed> Journal(string userDataFolder)
    {
        var path = JournalPath(userDataFolder);
        if (!File.Exists(path)) return [];
        var framed = new List<Framed>();
        if (ReadObject(path)?["framed"] is not JsonArray items) return framed;
        foreach (var item in items.OfType<JsonObject>())
        {
            var origin = Text(item, "origin");
            if (LabelOrigin(origin + "/_cage/enter") == origin) framed.Add(new Framed(origin, Text(item, "site")));
        }
        return framed;
    }

    public static bool Record(string userDataFolder, string origin, string site)
    {
        var framed = Journal(userDataFolder);
        if (framed.Any(item => item.Origin == origin)) return true;
        framed.Add(new Framed(origin, site));
        return WriteJournal(userDataFolder, framed);
    }

    public static bool Forget(string userDataFolder, IEnumerable<string> origins)
    {
        var gone = origins.ToHashSet(StringComparer.Ordinal);
        var framed = Journal(userDataFolder);
        if (!framed.Any(item => gone.Contains(item.Origin)) && (framed.Count != 0 || !File.Exists(JournalPath(userDataFolder)))) return true;
        return WriteJournal(userDataFolder, framed.Where(item => !gone.Contains(item.Origin)).ToList());
    }

    private static bool WriteJournal(string userDataFolder, List<Framed> framed)
    {
        var path = JournalPath(userDataFolder);
        try
        {
            if (framed.Count == 0)
            {
                File.Delete(path);
                return true;
            }
            Directory.CreateDirectory(userDataFolder);
            var items = new JsonArray(framed.Select(item => (JsonNode)new JsonObject { ["origin"] = item.Origin, ["site"] = item.Site }).ToArray());
            var temporary = path + ".cage-" + Guid.NewGuid().ToString("N");
            File.WriteAllText(temporary, new JsonObject { ["framed"] = items }.ToJsonString());
            File.Move(temporary, path, true);
            return true;
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException) { return false; }
    }

    // The locale override is per renderer process: a second target in the
    // same process reports the one already set.
    public static bool Benign(string message) => message.Contains("Another locale override is already in effect", StringComparison.Ordinal);

    // The proxy proof prepare runs (F5): an address that cannot exist, then a
    // loopback address outside the bypass list (round-2 N3), each loaded
    // under the window's own document without cookies. Only the switch's
    // proxy (127.0.0.1:9, where nothing listens) with the switch's bypass
    // list ends both with this error: any proxy that is down fails the first
    // the same way, but under another proxy (or none) the second goes direct.
    // The pane carries the verdict to the server: it is the window's word.
    public const string ProxyVerified = "net::ERR_PROXY_CONNECTION_FAILED";
    public const int ProxyCheckMilliseconds = 3000;
    public static string ProxyCheckURL(string nonce) => "http://cage-proxy-check.invalid/" + nonce;
    public static string ProxyLoopbackURL(string nonce) => "http://127.0.0.3:8099/" + nonce;
    public static JsonObject ProxyCheckOptions() => new() { ["disableCache"] = true, ["includeCredentials"] = false };
    public static string ProxyVerdict(bool success, string netErrorName) => !success && netErrorName == ProxyVerified ? "verified" : "failed";

    // PreferencesPath is the WebView's profile preferences under its user
    // data folder (WebView2 keeps the browser profile in EBWebView).
    public static string PreferencesPath(string userDataFolder) => Path.Combine(userDataFolder, "EBWebView", "Default", "Preferences");

    // SeedPreferences runs before the browser process starts: with the
    // switches, no network prediction (DNS prefetch, preconnect); without
    // them, the default again. A file it cannot read is left alone.
    public static bool SeedPreferences(string userDataFolder, bool on)
    {
        var path = PreferencesPath(userDataFolder);
        JsonObject root;
        try { root = File.Exists(path) ? JsonNode.Parse(File.ReadAllText(path)) as JsonObject ?? throw new JsonException("not an object") : new JsonObject(); }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException or JsonException) { return false; }
        var net = root["net"] as JsonObject;
        if (on)
        {
            if (net == null) root["net"] = net = new JsonObject();
            if (net["network_prediction_options"] is JsonValue current && current.TryGetValue<int>(out var value) && value == 2) return true;
            net["network_prediction_options"] = 2;
        }
        else
        {
            if (net?["network_prediction_options"] is not JsonValue current || !current.TryGetValue<int>(out var value) || value != 2) return true;
            net.Remove("network_prediction_options");
        }
        try
        {
            Directory.CreateDirectory(Path.GetDirectoryName(path)!);
            var temporary = path + ".cage-" + Guid.NewGuid().ToString("N");
            File.WriteAllText(temporary, root.ToJsonString());
            File.Move(temporary, path, true);
            return true;
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException) { return false; }
    }

    // Action is one thing the window does, in list order: calls on a session
    // ("" is the page), ending the frame (the pane falls back), ending the
    // front's session for the frame (EndFront), crashing a frame's process
    // (Crash, on Session), removing the frame element, or letting the held
    // navigation go.
    public sealed record Action(string Session, Call? Call, bool End = false, bool Remove = false, bool Proceed = false, bool EndFront = false, bool Crash = false);

    // Policy is what the window does with each DevTools target while host
    // rendering may show a frame (science_shell_test.go scienceFramePolicy):
    //  - Arm: the page session auto-attaches (paused) to every new target.
    //    Nothing on the workbench document itself changes.
    //  - A page-level iframe while armed is the Science frame: it gets the
    //    guest's identity and only then runs. Unarmed, it stays paused (and
    //    is prepared if the window arms again).
    //  - Any other page-level target is the workbench's own: resumed at once.
    //  - Under a guest target: workers get Worker, frames Frame, anything
    //    else stays paused and the frame ends.
    //  - A failed call never resumes its target. If the target detaches (it
    //    died), nothing more happens; if it is still attached when the window
    //    settles it, the frame ends. While the frame goes, a failed call ends
    //    nothing more.
    //  - Detach: the frame goes in one order (round-2 N1): EndFront (the
    //    front ends the frame's session, so nothing the frame still sends
    //    reaches the daemon), Crash for each page-level guest frame (its
    //    document and workers stop while their overrides are still in place),
    //    Remove. Auto-attach stays on until the last guest (and paused)
    //    target has detached. A second Detach while the frame goes repeats
    //    nothing.
    //  - A target's detach ends every target under it: a nested worker's
    //    session ends with its parent's without an event of its own (round-2
    //    N2). Listed (the page session's Target.getTargets) drops a
    //    page-level frame whose target is gone, with everything under it.
    //  - Navigate (the window's document is about to navigate): Detach, and
    //    the navigation is held until the targets are gone and auto-attach is
    //    off; if they do not go in time, the navigation is dropped and the
    //    frame ends. Arming while a navigation is held is refused.
    public sealed class Policy
    {
        private JsonObject? identity;
        private bool detaching, navigating;
        private readonly Dictionary<string, JsonObject> guests = new(StringComparer.Ordinal);
        private readonly SortedSet<string> held = new(StringComparer.Ordinal);
        private readonly HashSet<string> failed = new(StringComparer.Ordinal);
        // Parents ("" for page level) and target ids of every guest and held
        // target.
        private readonly Dictionary<string, string> parents = new(StringComparer.Ordinal);
        private readonly Dictionary<string, string> targets = new(StringComparer.Ordinal);

        public bool Attached { get; private set; }
        public bool Detaching => detaching;

        private static IEnumerable<Action> On(string session, IEnumerable<Call> calls) => calls.Select(call => new Action(session, call));

        public List<Action> Arm(JsonObject next)
        {
            if (navigating) return [new Action("", null, End: true)];
            identity = next;
            detaching = false;
            var actions = new List<Action>();
            if (!Attached)
            {
                Attached = true;
                actions.AddRange(On("", PageAttach));
            }
            foreach (var session in held.Where(session => parents.GetValueOrDefault(session) == "").ToList())
            {
                held.Remove(session);
                guests[session] = next;
                actions.AddRange(On(session, Frame(next)));
            }
            return actions;
        }

        public List<Action> Detach()
        {
            identity = null;
            if (!Attached) return [];
            if (detaching) return MaybeDetach();
            detaching = true;
            var actions = new List<Action> { new("", null, EndFront: true) };
            foreach (var session in guests.Keys.Where(session => parents.GetValueOrDefault(session) == "").Order(StringComparer.Ordinal))
                actions.Add(new Action(session, null, Crash: true));
            actions.Add(new Action("", null, Remove: true));
            actions.AddRange(MaybeDetach());
            return actions;
        }

        private List<Action> MaybeDetach()
        {
            if (!detaching || identity != null || guests.Count > 0 || held.Count > 0) return [];
            Attached = detaching = false;
            var actions = On("", PageDetach).ToList();
            if (navigating)
            {
                navigating = false;
                actions.Add(new Action("", null, Proceed: true));
            }
            return actions;
        }

        public List<Action> Navigate()
        {
            var actions = Detach();
            if (!Attached)
            {
                actions.Add(new Action("", null, Proceed: true));
                return actions;
            }
            navigating = true;
            return actions;
        }

        public List<Action> NavigateTimedOut()
        {
            if (!navigating) return [];
            navigating = false;
            return [new Action("", null, End: true)];
        }

        public List<Action> AttachedTarget(string parent, string session, string type, string target)
        {
            if (parent.Length == 0)
            {
                if (type != "iframe") return On(session, Resume).ToList();
                parents[session] = "";
                targets[session] = target;
                if (identity == null)
                {
                    held.Add(session);
                    return [];
                }
                guests[session] = identity;
                return On(session, Frame(identity)).ToList();
            }
            parents[session] = parent;
            targets[session] = target;
            if (!guests.TryGetValue(parent, out var inherited))
            {
                // Under a paused or unknown target: stays paused.
                held.Add(session);
                return [];
            }
            switch (type)
            {
                case "worker":
                    guests[session] = inherited;
                    return On(session, Worker(inherited)).ToList();
                case "iframe":
                    guests[session] = inherited;
                    return On(session, Frame(inherited)).ToList();
            }
            held.Add(session);
            return [new Action("", null, End: true)];
        }

        public List<Action> CallFailed(string session)
        {
            if (guests.ContainsKey(session)) failed.Add(session);
            return [];
        }

        public List<Action> Settle(string session)
        {
            if (!failed.Remove(session) || !guests.ContainsKey(session) || detaching || navigating) return [];
            return [new Action("", null, End: true)];
        }

        public List<Action> DetachedTarget(string session)
        {
            Forget(session);
            return MaybeDetach();
        }

        // Listed is the page session's Target.getTargets: a page-level frame
        // whose target is not listed is gone, with everything under it.
        public List<Action> Listed(IReadOnlyCollection<string> listed)
        {
            foreach (var session in parents.Where(pair => pair.Value.Length == 0 && !listed.Contains(targets.GetValueOrDefault(pair.Key, ""))).Select(pair => pair.Key).ToList())
                Forget(session);
            return MaybeDetach();
        }

        // Forget drops a target and every target under it.
        private void Forget(string session)
        {
            var gone = new HashSet<string>(StringComparer.Ordinal) { session };
            for (var changed = true; changed;)
            {
                changed = false;
                foreach (var (child, parent) in parents)
                    if (gone.Contains(parent) && gone.Add(child)) changed = true;
            }
            foreach (var item in gone)
            {
                guests.Remove(item);
                held.Remove(item);
                failed.Remove(item);
                parents.Remove(item);
                targets.Remove(item);
            }
        }
    }
}
