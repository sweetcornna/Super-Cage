// Browsing-data bridge for the guest controller (browser-data.mjs). It runs
// only when the controller evaluates cageBrowsingData on this hidden page:
// no listeners, no timers, no state between calls. It reaches the regular
// profile only; the controller's private and sign-in contexts are separate
// profiles that these APIs never see.
const historyLimit = 5000;
// Per entry, so a page that pushes long addresses cannot grow a reply past
// the host's limit; the whole address is fetched by id to open or delete it.
const urlLimit = 2048, titleLimit = 300;
const webURL = value => /^https?:\/\//.test(value || "");
const hostOf = value => { try { return new URL(value).hostname; } catch { return ""; } };
const within = (host, domain) => host === domain || host.endsWith("." + domain);
// Chromium refuses a whole removal for one origin it cannot parse
// ("https://www.192.168.1.10"): such origins are dropped and counted.
const validOrigins = list => list.filter(origin => { try { return new URL(origin).origin === origin; } catch { return false; } });
// The entry of one history id, found among the entries of its last visit.
const entryOf = async (id, time) => {
  time = Number(time) || 0;
  const items = await chrome.history.search({ text: "", startTime: time, endTime: time + 1, maxResults: 100 });
  return items.find(item => item.id === String(id));
};

globalThis.cageBrowsingData = async function (input) {
  switch (input.op) {
    case "probe":
      return { browsingData: typeof chrome.browsingData?.remove === "function", history: typeof chrome.history?.search === "function" };
    case "remove": {
      const options = { since: Number(input.since) || 0 };
      let skipped = 0;
      if (Array.isArray(input.origins)) {
        // A filter that lost every origin must never become a clear of all sites.
        const origins = validOrigins(input.origins);
        skipped = input.origins.length - origins.length;
        if (!origins.length) return { ok: true, skipped };
        options.origins = origins;
      } else if (input.excludeOrigins?.length) {
        const excluded = validOrigins(input.excludeOrigins);
        skipped = input.excludeOrigins.length - excluded.length;
        // Nor may an exclusion list that lost every origin become one.
        if (!excluded.length) return { ok: true, skipped };
        options.excludeOrigins = excluded;
      }
      await chrome.browsingData.remove(options, input.types);
      return { ok: true, skipped };
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
      return { items: items.filter(item => webURL(item.url)).map(item => ({ id: item.id, url: item.url.slice(0, urlLimit), title: (item.title || "").slice(0, titleLimit),
        time: item.lastVisitTime || 0, visits: item.visitCount || 0, ...(item.url.length > urlLimit ? { truncated: true } : {}) })), more: items.length >= input.limit };
    }
    case "history-url":
      return { url: (await entryOf(input.id, input.time))?.url || "" };
    case "history-delete-entries": {
      let removed = 0;
      for (const entry of input.entries) {
        const item = await entryOf(entry.id, entry.time);
        if (item) { await chrome.history.deleteUrl({ url: item.url }); removed++; }
      }
      return { ok: true, removed };
    }
    case "history-delete-urls":
      for (const url of input.urls) await chrome.history.deleteUrl({ url });
      return { ok: true, removed: input.urls.length };
    case "history-delete-range":
      await chrome.history.deleteRange({ startTime: input.start, endTime: input.end });
      return { ok: true };
    case "history-delete-host": {
      // history.search matches text anywhere in the URL or title, newest
      // first: walk the matches page by page (endTime) and delete those of the
      // host or its subdomains. Bounded at 100 pages of 1000.
      let removed = 0, end;
      for (let pass = 0; pass < 100; pass++) {
        const items = await chrome.history.search({ text: input.host, startTime: 0, maxResults: 1000, ...(end === undefined ? {} : { endTime: end }) });
        for (const item of items) if (within(hostOf(item.url), input.host)) { await chrome.history.deleteUrl({ url: item.url }); removed++; }
        if (items.length < 1000) return { ok: true, removed };
        end = Math.min(...items.map(item => item.lastVisitTime || 0));
      }
      return { ok: true, removed, more: true };
    }
    default:
      throw new Error("Unknown browsing data operation");
  }
};
