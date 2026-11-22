namespace Cage.Windows;

// WebGeneration ties the window's startup steps to the WebView they began
// in. Initialize begins a generation each time it creates a WebView (the
// first, a retry, the Claude Science restart); a step begun in an earlier
// one finds out after each await (IsCurrent) and leaves the window to the
// newer one: it navigates nothing, shows no error and holds up nothing.
// Only the window's thread calls it.
internal sealed class WebGeneration
{
    private int current, ready = -1, bootstrapping = -1, restarting = -1;

    // Begin starts a generation and returns it. Until it is Ready, no
    // Bootstrap starts.
    public int Begin() => ++current;

    public bool IsCurrent(int generation) => generation == current;

    // Ready: the generation's WebView has its handlers and bridge script.
    public void Ready(int generation)
    {
        if (generation == current) ready = generation;
    }

    // TryBootstrap starts one Bootstrap at a time in the current generation
    // once it is ready; one still waiting in an earlier generation does not
    // hold up the new one's.
    public bool TryBootstrap(out int generation)
    {
        generation = current;
        if (ready != current || bootstrapping == current) return false;
        bootstrapping = current;
        return true;
    }

    public void Bootstrapped(int generation)
    {
        if (bootstrapping == generation) bootstrapping = -1;
    }

    // TryRestart: one restart per generation; another request before the
    // restart begins the next generation is the same restart.
    public bool TryRestart()
    {
        if (restarting == current) return false;
        restarting = current;
        return true;
    }
}
