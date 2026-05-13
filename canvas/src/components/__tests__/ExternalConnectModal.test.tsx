'use client';

import { describe, it, expect } from 'vitest';
import {
  fillPythonSnippet,
  fillCurlSnippet,
  fillChannelSnippet,
  fillUniversalMcpSnippet,
  fillHermesSnippet,
  fillCodexSnippet,
  fillOpenClawSnippet,
  buildFilledSnippets,
  buildTabOrder,
  ExternalConnectionInfo,
} from '../ExternalConnectModal';

// ─── fillPythonSnippet ───────────────────────────────────────────────────────

describe('fillPythonSnippet', () => {
  it('stamps auth_token into the AUTH_TOKEN placeholder', () => {
    const input =
      'AUTH_TOKEN    = "<paste from create response>"\n' +
      'PLATFORM_URL  = "http://localhost:8080"';
    const got = fillPythonSnippet(input, 'tok-abc123');
    expect(got).toContain('AUTH_TOKEN    = "tok-abc123"');
    // Original placeholder is gone
    expect(got).not.toContain('<paste from create response>');
  });

  it('leaves other lines untouched', () => {
    const input = 'PLATFORM_URL = "http://localhost:8080"\nAUTH_TOKEN = "<paste from create response>"';
    const got = fillPythonSnippet(input, 'tok-xyz');
    expect(got).toContain('PLATFORM_URL = "http://localhost:8080"');
  });

  it('handles empty token', () => {
    const input = 'AUTH_TOKEN    = "<paste from create response>"';
    const got = fillPythonSnippet(input, '');
    expect(got).toContain('AUTH_TOKEN    = ""');
  });
});

// ─── fillCurlSnippet ─────────────────────────────────────────────────────────

describe('fillCurlSnippet', () => {
  it('stamps auth_token into WORKSPACE_AUTH_TOKEN placeholder', () => {
    const input = 'WORKSPACE_AUTH_TOKEN="<paste from create response>"';
    const got = fillCurlSnippet(input, 'tok-curl');
    expect(got).toContain('WORKSPACE_AUTH_TOKEN="tok-curl"');
    expect(got).not.toContain('<paste from create response>');
  });
});

// ─── fillChannelSnippet ─────────────────────────────────────────────────────

describe('fillChannelSnippet', () => {
  it('stamps token into MOLECULE_WORKSPACE_TOKENS placeholder', () => {
    const input = 'MOLECULE_WORKSPACE_TOKENS=<paste auth_token from create response>';
    const got = fillChannelSnippet(input, 'tok-channel');
    expect(got).toContain('MOLECULE_WORKSPACE_TOKENS=tok-channel');
  });

  it('returns undefined when snippet is undefined', () => {
    expect(fillChannelSnippet(undefined, 'tok')).toBeUndefined();
  });
});

// ─── fillUniversalMcpSnippet ───────────────────────────────────────────────

describe('fillUniversalMcpSnippet', () => {
  it('stamps token with double-quoted value', () => {
    const input = 'MOLECULE_WORKSPACE_TOKEN="<paste from create response>"';
    const got = fillUniversalMcpSnippet(input, 'tok-mcp');
    expect(got).toContain('MOLECULE_WORKSPACE_TOKEN="tok-mcp"');
  });

  it('returns undefined when snippet is undefined', () => {
    expect(fillUniversalMcpSnippet(undefined, 'tok')).toBeUndefined();
  });
});

// ─── fillHermesSnippet ─────────────────────────────────────────────────────

describe('fillHermesSnippet', () => {
  it('stamps token with double-quoted value', () => {
    const input = 'MOLECULE_WORKSPACE_TOKEN="<paste from create response>"';
    const got = fillHermesSnippet(input, 'tok-hermes');
    expect(got).toContain('MOLECULE_WORKSPACE_TOKEN="tok-hermes"');
  });

  it('returns undefined when snippet is undefined', () => {
    expect(fillHermesSnippet(undefined, 'tok')).toBeUndefined();
  });
});

// ─── fillCodexSnippet ──────────────────────────────────────────────────────

describe('fillCodexSnippet', () => {
  it('uses TOML spacing (space around equals)', () => {
    const input = 'MOLECULE_WORKSPACE_TOKEN = "<paste from create response>"';
    const got = fillCodexSnippet(input, 'tok-codex');
    expect(got).toContain('MOLECULE_WORKSPACE_TOKEN = "tok-codex"');
    expect(got).not.toContain('<paste from create response>');
  });

  it('returns undefined when snippet is undefined', () => {
    expect(fillCodexSnippet(undefined, 'tok')).toBeUndefined();
  });
});

// ─── fillOpenClawSnippet ───────────────────────────────────────────────────

describe('fillOpenClawSnippet', () => {
  it('stamps token with WORKSPACE_TOKEN key name', () => {
    const input = 'WORKSPACE_TOKEN="<paste from create response>"';
    const got = fillOpenClawSnippet(input, 'tok-oc');
    expect(got).toContain('WORKSPACE_TOKEN="tok-oc"');
    expect(got).not.toContain('<paste from create response>');
  });

  it('returns undefined when snippet is undefined', () => {
    expect(fillOpenClawSnippet(undefined, 'tok')).toBeUndefined();
  });
});

// ─── buildFilledSnippets ────────────────────────────────────────────────────

describe('buildFilledSnippets', () => {
  const makeInfo = (overrides: Partial<ExternalConnectionInfo> = {}): ExternalConnectionInfo =>
    ({
      workspace_id: 'ws-1',
      platform_url: 'http://localhost:8080',
      auth_token: 'tok-test',
      registry_endpoint: 'http://localhost:8080/registry/register',
      heartbeat_endpoint: 'http://localhost:8080/registry/heartbeat',
      python_snippet: 'AUTH_TOKEN    = "<paste from create response>"',
      curl_register_template: 'WORKSPACE_AUTH_TOKEN="<paste from create response>"',
      ...overrides,
    });

  it('fills python snippet', () => {
    const { filledPython } = buildFilledSnippets(makeInfo());
    expect(filledPython).toContain('tok-test');
  });

  it('fills curl snippet', () => {
    const { filledCurl } = buildFilledSnippets(makeInfo());
    expect(filledCurl).toContain('tok-test');
  });

  it('fills claude_code_channel_snippet when present', () => {
    const info = makeInfo({
      claude_code_channel_snippet: 'MOLECULE_WORKSPACE_TOKENS=<paste auth_token from create response>',
    });
    const { filledChannel } = buildFilledSnippets(info);
    expect(filledChannel).toContain('tok-test');
  });

  it('fills universal_mcp_snippet when present', () => {
    const info = makeInfo({
      universal_mcp_snippet: 'MOLECULE_WORKSPACE_TOKEN="<paste from create response>"',
    });
    const { filledUniversalMcp } = buildFilledSnippets(info);
    expect(filledUniversalMcp).toContain('tok-test');
  });

  it('fills hermes_channel_snippet when present', () => {
    const info = makeInfo({
      hermes_channel_snippet: 'MOLECULE_WORKSPACE_TOKEN="<paste from create response>"',
    });
    const { filledHermes } = buildFilledSnippets(info);
    expect(filledHermes).toContain('tok-test');
  });

  it('fills codex_snippet when present', () => {
    const info = makeInfo({
      codex_snippet: 'MOLECULE_WORKSPACE_TOKEN = "<paste from create response>"',
    });
    const { filledCodex } = buildFilledSnippets(info);
    expect(filledCodex).toContain('tok-test');
  });

  it('fills openclaw_snippet when present', () => {
    const info = makeInfo({
      openclaw_snippet: 'WORKSPACE_TOKEN="<paste from create response>"',
    });
    const { filledOpenClaw } = buildFilledSnippets(info);
    expect(filledOpenClaw).toContain('tok-test');
  });
});

// ─── buildTabOrder ──────────────────────────────────────────────────────────

describe('buildTabOrder', () => {
  const makeInfo = (overrides: Partial<ExternalConnectionInfo> = {}): ExternalConnectionInfo =>
    ({
      workspace_id: 'ws-1',
      platform_url: 'http://localhost:8080',
      auth_token: 'tok-test',
      registry_endpoint: 'http://localhost:8080/registry/register',
      heartbeat_endpoint: 'http://localhost:8080/registry/heartbeat',
      python_snippet: 'AUTH_TOKEN    = "<paste from create response>"',
      curl_register_template: 'WORKSPACE_AUTH_TOKEN="<paste from create response>"',
      ...overrides,
    });

  it('python is always present', () => {
    const tabs = buildTabOrder(makeInfo());
    expect(tabs).toContain('python');
  });

  it('curl and fields are always present', () => {
    const tabs = buildTabOrder(makeInfo());
    expect(tabs).toContain('curl');
    expect(tabs).toContain('fields');
  });

  it('mcp first when universal_mcp_snippet is present', () => {
    const tabs = buildTabOrder(makeInfo({
      universal_mcp_snippet: 'MOLECULE_WORKSPACE_TOKEN="<paste from create response>"',
    }));
    expect(tabs[0]).toBe('mcp');
  });

  it('python first when universal_mcp_snippet is absent', () => {
    const tabs = buildTabOrder(makeInfo());
    expect(tabs[0]).toBe('python');
  });

  it('mcp excluded when universal_mcp_snippet is absent', () => {
    const tabs = buildTabOrder(makeInfo());
    expect(tabs).not.toContain('mcp');
  });

  it('includes claude when claude_code_channel_snippet is present', () => {
    const tabs = buildTabOrder(makeInfo({
      claude_code_channel_snippet: 'MOLECULE_WORKSPACE_TOKENS=<paste auth_token from create response>',
    }));
    expect(tabs).toContain('claude');
  });

  it('includes hermes when hermes_channel_snippet is present', () => {
    const tabs = buildTabOrder(makeInfo({
      hermes_channel_snippet: 'MOLECULE_WORKSPACE_TOKEN="<paste from create response>"',
    }));
    expect(tabs).toContain('hermes');
  });

  it('includes codex when codex_snippet is present', () => {
    const tabs = buildTabOrder(makeInfo({
      codex_snippet: 'MOLECULE_WORKSPACE_TOKEN = "<paste from create response>"',
    }));
    expect(tabs).toContain('codex');
  });

  it('includes openclaw when openclaw_snippet is present', () => {
    const tabs = buildTabOrder(makeInfo({
      openclaw_snippet: 'WORKSPACE_TOKEN="<paste from create response>"',
    }));
    expect(tabs).toContain('openclaw');
  });

  it('all optional tabs at once: full house', () => {
    const tabs = buildTabOrder(makeInfo({
      universal_mcp_snippet: 'MOLECULE_WORKSPACE_TOKEN="<paste from create response>"',
      claude_code_channel_snippet: 'MOLECULE_WORKSPACE_TOKENS=<paste auth_token from create response>',
      hermes_channel_snippet: 'MOLECULE_WORKSPACE_TOKEN="<paste from create response>"',
      codex_snippet: 'MOLECULE_WORKSPACE_TOKEN = "<paste from create response>"',
      openclaw_snippet: 'WORKSPACE_TOKEN="<paste from create response>"',
    }));
    expect(tabs).toEqual([
      'mcp', 'python', 'claude', 'hermes', 'codex', 'openclaw', 'curl', 'fields',
    ]);
  });
});
