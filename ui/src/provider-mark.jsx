import React from "react";
import { HarnessMark, harnessMarkId } from "./agent-icon";

// Keep the <i> wrapper so a session row's first <span> remains its title.
export function ProviderMark({ provider = "claude", accountNumber, size = 16, className = "", decorative = false }) {
  const label = harnessMarkId(provider) === "claude" ? "Claude" : undefined;
  return <HarnessMark agentId={provider} accountNumber={accountNumber} size={size} className={className}
    label={label} decorative={decorative} slot="provider-mark" />;
}
