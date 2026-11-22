import { useEffect, useState } from "react";
import { api } from "./api";
import { getClientContext } from "./client-context.mjs";
import { AGENTS_PATH, navRowVisible, policyHidesRow } from "./other-clients-nav.mjs";

// Whether 设置 shows the 其他客户端 row: it appears only when something can be
// added in this environment (a build whose optional entries are all hidden, a
// server that allows none, or a host without the routes keeps it away). Asked
// once when the settings open (a plain read: no guest is asked), never at load
// and never again; until the answer arrives, and if it fails, the row is absent.
export function useOtherClientsRow() {
  const [visible, setVisible] = useState(false);
  useEffect(() => {
    const policy = getClientContext().agents_policy;
    if (policyHidesRow(policy)) return undefined;
    const controller = new AbortController();
    api(AGENTS_PATH, undefined, undefined, {}, { signal: controller.signal })
      .then((answer) => setVisible(navRowVisible(answer, policy)))
      .catch(() => {});
    return () => controller.abort();
  }, []);
  return visible;
}
