// Browsing-data bridge for the guest controller (browser-data.mjs). It runs
// only when the controller evaluates cageBrowsingData on this hidden page:
// no listeners, no timers, no state between calls. It reaches the regular
// profile only; the controller's private and sign-in contexts are separate
// profiles that these APIs never see.
const historyLimit = 5000;
const webURL = value => /^https?:\/\//.test(value || "");
const hostOf = value => { try { return new URL(value).hostname; } catch { return ""; } };
const within = (host, domain) => host === domain || host.endsWith("." + domain);

globalThis.cageBrowsingData = async function (input) {
  switch (input.op) {
    case "probe":
      return { browsingData: typeof chrome.browsingData?.remove === "function", history: typeof chrome.history?.search === "function" };
    case "remove": {
      const options = { since: Number(input.since) || 0 };
      if (input.origins?.length) options.origins = input.origins;
      else if (input.excludeOrigins?.length) options.excludeOrigins = input.excludeOrigins;
      await chrome.browsingData.remove(options, input.types);
      return { ok: true };
    }
    case "history-count": {
      const items = await chrome.history.search({ text: "", startTime: Number(input.since) || 0, maxResults: input.limit });
      return { count: items.filter(item => webURL(item.url)).length, more: items.length >= input.limit };
    }
    case "history-origins": {
      // Aggregated here so a long history never crosses the pipe as URLs.
      const items = await chrome.history.search({ text: "", startTime: 0, maxResults: historyLimit });
      const origins = {};
      for (const item of items) {
        if (!webURL(item.url)) continue;
        let origin;
        try { origin = new URL(item.url).origin; } catch { continue; }
        const entry = origins[origin] ||= { visits: 0, last: 0 };
        entry.visits += item.visitCount || 1;
        entry.last = Math.max(entry.last, item.lastVisitTime || 0);
      }
      return { origins, complete: items.length < historyLimit };
    }
    case "history-page": {
      const query = { text: input.query || "", startTime: Number(input.start) || 0, maxResults: input.limit };
      if (input.end) query.endTime = input.end;
      const items = await chrome.history.search(query);
      return { items: items.filter(item => webURL(item.url)).map(item => ({ url: item.url, title: item.title || "", time: item.lastVisitTime || 0, visits: item.visitCount || 0 })), more: items.length >= input.limit };
    }
    case "history-delete-urls":
      for (const url of input.urls) await chrome.history.deleteUrl({ url });
      return { ok: true, removed: input.urls.length };
    case "history-delete-range":
      await chrome.history.deleteRange({ startTime: input.start, endTime: input.end });
      return { ok: true };
    case "history-delete-host": {
      // history.search matches text anywhere in the URL or title; keep only
      // entries of the host or its subdomains. Bounded passes of 1000.
      let removed = 0;
      for (let pass = 0; pass < 20; pass++) {
        const items = await chrome.history.search({ text: input.host, startTime: 0, maxResults: 1000 });
        const matching = items.filter(item => within(hostOf(item.url), input.host));
        for (const item of matching) await chrome.history.deleteUrl({ url: item.url });
        removed += matching.length;
        if (items.length < 1000 || !matching.length) break;
      }
      return { ok: true, removed };
    }
    default:
      throw new Error("Unknown browsing data operation");
  }
};
