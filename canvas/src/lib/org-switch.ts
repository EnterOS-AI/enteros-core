// Org switching across tenant subdomains.
//
// Each org is its own tenant at <slug>.<apex> (e.g. agents-team.moleculesai.app),
// so switching orgs from the canvas topbar means navigating to the target org's
// subdomain. switchOrgUrl derives that URL from the current location, or returns
// null when it's a no-op (same org / empty target) or the apex can't be resolved.

export function switchOrgUrl(
  hostname: string,
  protocol: string,
  currentSlug: string,
  targetSlug: string,
): string | null {
  if (!targetSlug || targetSlug === currentSlug) return null;
  // Prefer stripping the known current-org label; otherwise drop the first
  // label as a best-effort apex (covers hosts we didn't seed a slug for).
  const apex =
    currentSlug && hostname.startsWith(`${currentSlug}.`)
      ? hostname.slice(currentSlug.length + 1)
      : hostname.split(".").slice(1).join(".");
  if (!apex) return null;
  return `${protocol}//${targetSlug}.${apex}`;
}
