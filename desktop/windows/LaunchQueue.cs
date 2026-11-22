namespace Cage.Windows;

// LaunchQueue runs the window's backend launches (cage desktop --json) one at
// a time: never two of its processes at once. A launch asked for while the
// same one (same key: the same arguments) is queued or running gets that
// launch's reply instead of a process of its own: Retry after the WebView
// crashed during a slow start, the page's refreshSession during a Bootstrap.
// A different launch waits for the one in flight; a failed launch does not
// hold up the next. Whether a reply is still for the current WebView is the
// caller's business (WebGeneration). Only the window's thread calls Run.
internal sealed class LaunchQueue<T>
{
    private readonly SemaphoreSlim one = new(1, 1);
    private string? latestKey;
    private Task<T>? latest;

    public Task<T> Run(string key, Func<Task<T>> launch)
    {
        if (latest is { IsCompleted: false } && latestKey == key) return latest;
        latestKey = key;
        return latest = Queued(launch);
    }

    private async Task<T> Queued(Func<Task<T>> launch)
    {
        await one.WaitAsync();
        try { return await launch(); }
        finally { one.Release(); }
    }
}
