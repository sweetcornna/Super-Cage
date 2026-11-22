namespace Cage.Windows;

// ScienceSmoke decides what the Claude Science part of a smoke test drives
// (MainWindow.SmokeReady). CAGE_SMOKE_SCIENCE=on|off changes the stored
// host-render switch from the page and =probe asks the frame ops what the
// pane would; any of them only in smoke mode (--smoke-test, which sets
// Program.SmokeOutput) and never on the default profile, whose switch is
// the person's own.
internal static class ScienceSmoke
{
    public const string Variable = "CAGE_SMOKE_SCIENCE";

    // Of returns the mode ("on", "off", "probe", or "" for none) and whether
    // a mode was asked for and refused because root is the default profile
    // (home's .claude-cage) or inside it.
    public static (string Mode, bool Refused) Of(string? smokeOutput, string? requested, string root, string home)
    {
        if (smokeOutput == null || requested is not ("on" or "off" or "probe")) return ("", false);
        return Within(root, Path.Combine(home, ".claude-cage")) ? ("", true) : (requested, false);
    }

    // Within: path is folder or under it, compared as full paths on a
    // separator boundary (.claude-cage-smoke is not under .claude-cage),
    // case-insensitively on Windows.
    public static bool Within(string path, string folder)
    {
        var comparison = OperatingSystem.IsWindows() ? StringComparison.OrdinalIgnoreCase : StringComparison.Ordinal;
        var full = Path.TrimEndingDirectorySeparator(Path.GetFullPath(path));
        var parent = Path.TrimEndingDirectorySeparator(Path.GetFullPath(folder));
        return full.Equals(parent, comparison) || full.StartsWith(parent + Path.DirectorySeparatorChar, comparison);
    }
}
