// @vitest-environment jsdom
/**
 * Tests for readThemeCookie — parses a cookie value into a ThemePreference —
 * and brandCookieDomain — derives the theme cookie's Domain= apex from the
 * runtime hostname across BOTH brand generations (Enter OS rebrand,
 * internal#1089 Phase 2).
 */
import { describe, it, expect } from "vitest";
import {
  BrandCookieDomains,
  brandCookieDomain,
  readThemeCookie,
} from "../theme-cookie";

describe("readThemeCookie", () => {
  it('returns "light" when cookie value is "light"', () => {
    expect(readThemeCookie("light")).toBe("light");
  });

  it('returns "dark" when cookie value is "dark"', () => {
    expect(readThemeCookie("dark")).toBe("dark");
  });

  it('returns "system" when cookie value is "system"', () => {
    expect(readThemeCookie("system")).toBe("system");
  });

  it('returns "system" for undefined', () => {
    expect(readThemeCookie(undefined)).toBe("system");
  });

  it('returns "system" for empty string', () => {
    expect(readThemeCookie("")).toBe("system");
  });

  it('returns "system" for any non-matching value', () => {
    expect(readThemeCookie("auto")).toBe("system");
    expect(readThemeCookie("dark-mode")).toBe("system");
    expect(readThemeCookie("DARK")).toBe("system"); // case-sensitive
    expect(readThemeCookie("light\n")).toBe("system"); // whitespace-sensitive
    expect(readThemeCookie("  system  ")).toBe("system");
    expect(readThemeCookie("null")).toBe("system");
    expect(readThemeCookie("0")).toBe("system");
  });

  it("is pure — same input always returns same output", () => {
    const inputs = ["light", "dark", "system", undefined, ""];
    for (const input of inputs) {
      for (let i = 0; i < 3; i++) {
        expect(readThemeCookie(input)).toBe(readThemeCookie(input));
      }
    }
  });
});

describe("brandCookieDomain (internal#1089 Phase 2 dual-domain)", () => {
  it("keeps the exact legacy behavior on moleculesai.app hosts", () => {
    expect(brandCookieDomain("acme.moleculesai.app")).toBe(".moleculesai.app");
    expect(brandCookieDomain("app.moleculesai.app")).toBe(".moleculesai.app");
    // Staging tenants are deeper subdomains of the same apex.
    expect(brandCookieDomain("acme.staging.moleculesai.app")).toBe(
      ".moleculesai.app",
    );
  });

  it("recognizes the new enteros.ai brand generation", () => {
    expect(brandCookieDomain("acme.enteros.ai")).toBe(".enteros.ai");
    expect(brandCookieDomain("acme.staging.enteros.ai")).toBe(".enteros.ai");
  });

  it("never cross-assigns one brand's apex to the other's host", () => {
    // The failure this exists to prevent: a baked `.moleculesai.app` Domain
    // on an enteros.ai host (browser would drop the attribute silently).
    expect(brandCookieDomain("acme.enteros.ai")).not.toBe(".moleculesai.app");
    expect(brandCookieDomain("acme.moleculesai.app")).not.toBe(".enteros.ai");
  });

  it("returns null off both brand domains (host-only cookie, as before)", () => {
    expect(brandCookieDomain("localhost")).toBeNull();
    expect(brandCookieDomain("acme.example.com")).toBeNull();
    // Suffix must be a REAL subdomain match, not a substring trick.
    expect(brandCookieDomain("evil-moleculesai.app")).toBeNull();
    expect(brandCookieDomain("evilenteros.ai")).toBeNull();
    // The bare apex itself has no leading-dot suffix match (unchanged from
    // the old endsWith(".moleculesai.app") literal behavior).
    expect(brandCookieDomain("moleculesai.app")).toBeNull();
    expect(brandCookieDomain("enteros.ai")).toBeNull();
  });

  it("is case-insensitive on the hostname", () => {
    expect(brandCookieDomain("Acme.EnterOS.ai")).toBe(".enteros.ai");
  });

  it("derives every answer from the ONE BrandCookieDomains list", () => {
    // Shape guard (mirrors ResourcePrefix/LegacyResourcePrefixes): both brand
    // generations present, legacy entry retained, leading dots everywhere.
    expect(BrandCookieDomains).toContain(".moleculesai.app");
    expect(BrandCookieDomains).toContain(".enteros.ai");
    for (const d of BrandCookieDomains) {
      expect(d.startsWith(".")).toBe(true);
      expect(brandCookieDomain(`tenant${d}`)).toBe(d);
    }
  });
});
