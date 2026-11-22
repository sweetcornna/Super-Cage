namespace Cage.Windows;

// CrashReports keeps the window's crash dumps off the disk. The window's
// WebView2 environment runs with custom crash reporting (MainWindow): no
// crash report goes to Microsoft, and the runtime still writes its dumps (a
// process's memory: the workbench's tokens, a Claude Science page) under the
// user data folder. Sweep deletes them: before every WebView start, and after
// a Science frame ended through Page.crash (ScienceFrameHost).
internal static class CrashReports
{
    // Folder is where WebView2 keeps the crash reports of a user data folder.
    public static string Folder(string userDataFolder) => Path.Combine(userDataFolder, "EBWebView", "Crashpad");

    // Sweep deletes the dumps in reports and everything in attachments; the
    // crash database's own files stay. A file in use stays for the next
    // sweep. Nothing is followed through a link: when the user data folder,
    // EBWebView, Crashpad or the folder it walks is a reparse point (a
    // junction or a symbolic link), that part is not swept. Returns how many
    // files it deleted.
    public static int Sweep(string userDataFolder)
    {
        var crashpad = Folder(userDataFolder);
        try
        {
            if (!Directory.Exists(crashpad)) return 0;
            foreach (var folder in new[] { userDataFolder, Path.GetDirectoryName(crashpad)!, crashpad })
                if (Linked(folder)) return 0;
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException) { return 0; }
        var deleted = 0;
        foreach (var (folder, pattern) in new[] { (Path.Combine(crashpad, "reports"), "*.dmp"), (Path.Combine(crashpad, "attachments"), "*") })
        {
            List<string> files;
            try
            {
                if (!Directory.Exists(folder) || Linked(folder)) continue;
                files = Directory.EnumerateFiles(folder, pattern, new EnumerationOptions { RecurseSubdirectories = true, AttributesToSkip = FileAttributes.ReparsePoint }).ToList();
            }
            catch (Exception error) when (error is IOException or UnauthorizedAccessException) { continue; }
            foreach (var file in files)
            {
                try
                {
                    File.Delete(file);
                    deleted++;
                }
                catch (Exception error) when (error is IOException or UnauthorizedAccessException) { }
            }
        }
        return deleted;
    }

    private static bool Linked(string folder) => File.GetAttributes(folder).HasFlag(FileAttributes.ReparsePoint);
}
