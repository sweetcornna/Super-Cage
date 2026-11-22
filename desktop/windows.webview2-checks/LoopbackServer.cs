using System.Net;
using System.Net.Sockets;
using System.Text;

namespace Cage.Windows;

// LoopbackServer is a minimal HTTP/1.1 server on 127.0.0.1 and [::1] (one
// port): enough for the checks' workbench and label documents. Handle
// answers (status, content type, body) for a request.
internal sealed class LoopbackServer : IDisposable
{
    public sealed record Request(string Method, string Path, string Host, string Body, IReadOnlyDictionary<string, string> Headers);
    public sealed record Response(int Status, string Type, string Body, IReadOnlyDictionary<string, string>? Headers = null);

    private readonly List<TcpListener> listeners = [];
    private readonly Func<Request, Response> handle;
    private readonly CancellationTokenSource stop = new();
    public int Port { get; }

    public LoopbackServer(Func<Request, Response> handle)
    {
        this.handle = handle;
        var first = new TcpListener(IPAddress.Loopback, 0);
        first.Start();
        Port = ((IPEndPoint)first.LocalEndpoint).Port;
        listeners.Add(first);
        try
        {
            var second = new TcpListener(IPAddress.IPv6Loopback, Port);
            second.Start();
            listeners.Add(second);
        }
        catch (SocketException) { }
        foreach (var listener in listeners) _ = Accept(listener);
    }

    private async Task Accept(TcpListener listener)
    {
        while (!stop.IsCancellationRequested)
        {
            TcpClient client;
            try { client = await listener.AcceptTcpClientAsync(stop.Token); }
            catch (Exception) { return; }
            _ = Serve(client);
        }
    }

    private async Task Serve(TcpClient client)
    {
        using var _ = client;
        try
        {
            var stream = client.GetStream();
            var buffer = new List<byte>();
            var chunk = new byte[8192];
            while (!stop.IsCancellationRequested)
            {
                int end;
                while ((end = HeadEnd(buffer)) < 0)
                {
                    var read = await stream.ReadAsync(chunk, stop.Token);
                    if (read == 0) return;
                    buffer.AddRange(chunk.AsSpan(0, read).ToArray());
                }
                var head = Encoding.ASCII.GetString(buffer.GetRange(0, end).ToArray()).Split("\r\n");
                buffer.RemoveRange(0, end + 4);
                var line = head[0].Split(' ');
                var headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
                foreach (var header in head.Skip(1))
                {
                    var colon = header.IndexOf(':');
                    if (colon > 0) headers[header[..colon].Trim()] = header[(colon + 1)..].Trim();
                }
                var length = headers.TryGetValue("Content-Length", out var value) && int.TryParse(value, out var number) ? number : 0;
                while (buffer.Count < length)
                {
                    var read = await stream.ReadAsync(chunk, stop.Token);
                    if (read == 0) return;
                    buffer.AddRange(chunk.AsSpan(0, read).ToArray());
                }
                var body = Encoding.UTF8.GetString(buffer.GetRange(0, length).ToArray());
                buffer.RemoveRange(0, length);
                Response response;
                try { response = handle(new Request(line[0], line.Length > 1 ? line[1] : "/", headers.GetValueOrDefault("Host", ""), body, headers)); }
                catch (Exception error) { response = new Response(500, "text/plain", error.Message); }
                var payload = Encoding.UTF8.GetBytes(response.Body);
                var text = new StringBuilder();
                text.Append("HTTP/1.1 ").Append(response.Status).Append(' ').Append(response.Status == 200 ? "OK" : "Status").Append("\r\n");
                text.Append("Content-Type: ").Append(response.Type).Append("\r\n");
                text.Append("Content-Length: ").Append(payload.Length).Append("\r\n");
                text.Append("Cache-Control: no-store\r\n");
                foreach (var (name, header) in response.Headers ?? new Dictionary<string, string>()) text.Append(name).Append(": ").Append(header).Append("\r\n");
                text.Append("\r\n");
                await stream.WriteAsync(Encoding.ASCII.GetBytes(text.ToString()), stop.Token);
                await stream.WriteAsync(payload, stop.Token);
            }
        }
        catch (Exception) { }
    }

    private static int HeadEnd(List<byte> buffer)
    {
        for (var i = 0; i + 3 < buffer.Count; i++)
            if (buffer[i] == '\r' && buffer[i + 1] == '\n' && buffer[i + 2] == '\r' && buffer[i + 3] == '\n') return i;
        return -1;
    }

    public void Dispose()
    {
        stop.Cancel();
        foreach (var listener in listeners) listener.Stop();
    }
}
