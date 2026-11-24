import test from "node:test";
import assert from "node:assert/strict";
import {
  agentLoginLinkRegex,
  claudeLoginLink,
  isAgentLoginLink,
  isClaudeLoginLink,
} from "./terminal-login-link.mjs";

test("Claude login link validation remains strict and backward compatible", () => {
  const valid = "https://claude.ai/oauth/authorize?code=true&client_id=abc";
  assert.equal(isClaudeLoginLink(valid), true);
  assert.equal(isAgentLoginLink(valid), true);
  assert.equal(claudeLoginLink.test(valid), true);
  assert.equal(isClaudeLoginLink("https://claude.ai/chats"), false);
  assert.equal(isAgentLoginLink("https://claude.ai/chats"), false);
});

test("Mainstream and custom agent OAuth/device links are matched while untrusted and regular links are rejected", () => {
  const allowed = [
    "https://auth.openai.com/oauth/authorize?client_id=codex",
    "https://platform.openai.com/auth/login",
    "https://accounts.google.com/o/oauth2/v2/auth?scope=openid",
    "https://aistudio.google.com/oauth/authorize?client_id=gemini",
    "https://github.com/login/device",
    "https://auth.githubcopilot.com/oauth/device",
    "https://chat.qwen.ai/authorize?user_code=ABCD-1234",
    "https://account.aliyun.com/login/oauth",
    "https://ampcode.com/auth/cli-login",
    "https://auth.cline.bot/login?state=1",
    "https://auth.mistral.ai/oauth/authorize?client_id=vibe",
    "https://code.kimi.com/auth/device?code=1234",
    "https://auth.factory.ai/oauth/authorize?client_id=droid",
    "https://pi.dev/login?code=xyz",
    "https://openrouter.ai/auth?callback_url=http://localhost",
  ];
  for (const url of allowed) {
    assert.equal(isAgentLoginLink(url), true, `expected allowed: ${url}`);
    assert.equal(agentLoginLinkRegex().exec(url)?.[0], url);
  }

  // Custom agent OAuth host
  const customURL = "https://sso.internal-agent.example/oauth/authorize?state=1";
  assert.equal(isAgentLoginLink(customURL), false);
  assert.equal(isAgentLoginLink(customURL, ["sso.internal-agent.example"]), true);
  assert.equal(agentLoginLinkRegex(["sso.internal-agent.example"]).exec(customURL)?.[0], customURL);

  // Reject unsafe schemes, ports, userinfo, untrusted hosts, and regular repository/query links
  const rejected = [
    "http://auth.openai.com/oauth/authorize?client_id=codex",
    "https://evil.example/oauth/authorize?client_id=codex",
    "https://user:pass@auth.openai.com/oauth/authorize?x=1",
    "https://auth.openai.com:443/oauth/authorize?x=1",
    "  https://auth.openai.com:443/oauth/authorize?x=1  ",
    "https://auth.openai.com:0443/oauth/authorize?x=1",
    "https:\\\\auth.openai.com:443/oauth/authorize?x=1",
    "https://auth.openai.com:8443/oauth/authorize?x=1",
    "https://claude.ai:443/oauth/authorize?client_id=abc",
    "https://github.com/octocat/hello-world",
    "https://github.com/org/repo?tab=readme",
    "https://github.com/org/repo/issues?q=is%3Aopen",
    "https://chatgpt.com/c/12345?model=o3",
  ];
  for (const url of rejected) {
    assert.equal(isAgentLoginLink(url), false, `expected rejected: ${url}`);
  }
});
