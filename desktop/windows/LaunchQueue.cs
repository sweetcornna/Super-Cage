namespace Cage.Windows;

// LaunchQueue runs the window's backend launches (cage desktop --json) one at
// a time: never two of its processes at once. A launch asked for while the
// same one (same key) is queued or running gets that launch's reply instead
// of a process of its own (Joined): Retry after the WebView crashed during a
// slow start. A launch without a key never joins and is never joined; it
// waits for the one in flight, as does a launch with another key. A failed
// launch does not hold up the next. Whether a reply is still for the
// current WebView is the caller's business (WebGeneration). Only the
// window's thread calls Run.
internal sealed class LaunchQueue<T>
{
    private readonly SemaphoreSlim one = new(1, 1);
    private string? latestKey;
    private Task<T>? latest;

    // Joined: how many launches got the reply of one in flight.
    public int Joined { get; private set; }

    public Task<T> Run(string? key, Func<Task<T>> launch)
    {
        if (key != null && latest is { IsCompleted: false } && latestKey == key)
        {
            Joined++;
            return latest;
        }
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
