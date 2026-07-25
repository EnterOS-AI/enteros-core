import type { Metadata } from "next";
import localFont from "next/font/local";
import { cookies, headers } from "next/headers";
import "./globals.css";

// Enter OS brand typography (internal#1089). The woff2 files in ./fonts/
// are byte-identical copies of molecule-ai-sdk brand/assets/fonts/ (the
// brand SSOT — Chillax is Fontshare, not on Google Fonts, so next/font/local
// instead of next/font/google). Self-hosted at build time → CSP-safe
// (font-src 'self' covers them because Next.js serves the .woff2 from
// /_next/static). Exposed as CSS variables so the mobile palette and
// CSS modules can reference them without importing this module:
//   --font-chillax     → display headings (--font-display in globals.css)
//   --font-inter-tight → body sans (--font-sans in globals.css)
//   --font-jetbrains   → mono (--font-mono in globals.css)
const displayFont = localFont({
  src: [
    { path: "./fonts/Chillax-Light.woff2", weight: "300", style: "normal" },
    { path: "./fonts/Chillax-Regular.woff2", weight: "400", style: "normal" },
    { path: "./fonts/Chillax-Medium.woff2", weight: "500", style: "normal" },
    { path: "./fonts/Chillax-Semibold.woff2", weight: "600", style: "normal" },
  ],
  display: "swap",
  variable: "--font-chillax",
});
const sansFont = localFont({
  src: [
    { path: "./fonts/InterTight-Regular.woff2", weight: "400", style: "normal" },
    { path: "./fonts/InterTight-Medium.woff2", weight: "500", style: "normal" },
  ],
  display: "swap",
  variable: "--font-inter-tight",
});
const monoFont = localFont({
  src: [
    { path: "./fonts/JetBrainsMono-Regular.woff2", weight: "400", style: "normal" },
    { path: "./fonts/JetBrainsMono-Medium.woff2", weight: "500", style: "normal" },
  ],
  display: "swap",
  variable: "--font-jetbrains",
});
import { AuthGate } from "@/components/AuthGate";
import { CookieConsent } from "@/components/CookieConsent";
import { PurchaseSuccessModal } from "@/components/PurchaseSuccessModal";
import { ErrorBoundary } from "@/components/ErrorBoundary";
import { ThemeProvider } from "@/lib/theme-provider";
import {
  THEME_COOKIE,
  readThemeCookie,
  themeBootScript,
} from "@/lib/theme-cookie";

// Marketing-launch SEO (mc#1486). Canonical apex is app.moleculesai.app —
// tenant subdomains (<slug>.moleculesai.app) reuse the same Next.js build
// but are gated behind auth (AuthGate redirects anonymous → /cp/auth/login)
// and are de-indexed in robots.ts. The metadata here applies to the
// public marketing surface served from the apex host.
//
// Override per-route by exporting a page-level `metadata`/`generateMetadata`
// — Next.js merges page metadata over layout metadata using
// `title.template` for "<page> | Enter OS" composition.
const SITE_URL =
  process.env.NEXT_PUBLIC_SITE_URL ?? "https://app.moleculesai.app";

export const metadata: Metadata = {
  metadataBase: new URL(SITE_URL),
  title: {
    default: "Enter OS — the AI org chart canvas",
    template: "%s | Enter OS",
  },
  description:
    "Enter OS is an org-chart canvas for AI agent teams. Wire Claude Code, Codex, Hermes, and OpenClaw agents into a governed multi-agent workspace with credit metering, audit, and one-click runtime provisioning.",
  applicationName: "Enter OS",
  keywords: [
    "AI agents",
    "multi-agent",
    "agent orchestration",
    "AI org chart",
    "Claude Code",
    "Codex",
    "MCP",
    "agent governance",
    "A2A",
    "agent runtime",
  ],
  authors: [{ name: "Enter OS" }],
  creator: "Enter OS",
  publisher: "Enter OS",
  alternates: { canonical: "/" },
  // OG + Twitter images come from the file-convention sibling
  // `opengraph-image.tsx` — Next.js auto-attaches them to og:image
  // and twitter:image when present at the segment root. We keep the
  // text fields here so they win over per-page metadata when a page
  // doesn't override them. `images: []` as the structural fallback
  // for hosts that won't follow the file convention; the real URL
  // is injected by Next.js at build time from opengraph-image.tsx.
  openGraph: {
    type: "website",
    siteName: "Enter OS",
    url: SITE_URL,
    title: "Enter OS — the AI org chart canvas",
    description:
      "Wire Claude Code, Codex, Hermes, and OpenClaw agents into a governed multi-agent workspace. Credit metering, audit, and one-click runtime provisioning.",
    locale: "en_US",
  },
  twitter: {
    card: "summary_large_image",
    title: "Enter OS — the AI org chart canvas",
    description:
      "Wire Claude Code, Codex, Hermes, and OpenClaw agents into a governed multi-agent workspace.",
  },
  icons: {
    icon: "/molecule-icon.png",
    apple: "/molecule-icon.png",
  },
  // robots.ts owns the per-route allow/disallow contract; this is the
  // header-level fallback for routes the crawler reaches before
  // robots.txt resolves. Default = index public marketing routes;
  // app/auth/api/orgs are noindex'd by robots.ts.
  robots: {
    index: true,
    follow: true,
    googleBot: { index: true, follow: true, "max-image-preview": "large" },
  },
};

export default async function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  // Read the per-request CSP nonce that middleware.ts sets via the
  // `x-nonce` request header. This call is load-bearing for THREE
  // independent reasons:
  //
  //   1. It opts the root layout into dynamic rendering. Without a
  //      `headers()` / `cookies()` / `noStore()` call, Next.js treats
  //      the layout as statically pre-rendered and serves the SAME
  //      HTML for every request — which means the Next.js bootstrap
  //      <script> tags bake into the HTML without any nonce. The
  //      browser then rejects every one with a CSP violation because
  //      the header demands nonce-only script execution.
  //
  //   2. Next.js 15 propagates the nonce to its own generated inline
  //      scripts (the __next_f chunk push frames) ONLY when the header
  //      is actually read via `headers()`. The header's existence on
  //      the request isn't enough — Next.js watches for the read.
  //
  //   3. We need the nonce to attach to the inline theme boot script
  //      below, otherwise CSP rejects it in production where
  //      script-src is `'self' 'nonce-{nonce}' 'strict-dynamic'`.
  //      'strict-dynamic' propagates trust from a nonce'd script to
  //      scripts it inserts, but does NOT forgive an un-nonce'd
  //      sibling — the boot script must carry its own nonce.
  const hdrs = await headers();
  const nonce = hdrs.get("x-nonce") ?? undefined;

  // SSR: read the user's saved preference. For light/dark we can stamp
  // data-theme on <html> here so the very first paint matches; for
  // "system" we leave the attribute off and let the inline boot script
  // resolve from matchMedia before paint.
  const cookieStore = await cookies();
  const theme = readThemeCookie(cookieStore.get(THEME_COOKIE)?.value);
  const initialDataTheme = theme === "system" ? undefined : theme;

  return (
    // suppressHydrationWarning on <html>: the inline boot script below
    // mutates `data-theme` before React hydrates (system mode reads
    // matchMedia + writes the attribute). That's the entire point of the
    // script — eliminate the flash — and it's the documented escape hatch
    // for "the server-rendered HTML is intentionally not what React would
    // produce client-side at this exact attribute."
    <html lang="en" data-theme={initialDataTheme} suppressHydrationWarning>
      <head>
        {/*
         * Boot script: runs synchronously before the body paints, sets
         * data-theme on <html> for "system" preference based on the OS
         * media query. For explicit light/dark, SSR already set the
         * attribute above and the script's write is a no-op.
         *
         * `nonce` comes from middleware's per-request CSP nonce — see
         * the comment block above for why CSP requires this even though
         * the page also has 'strict-dynamic'.
         */}
        <script
          nonce={nonce}
          // The browser strips the nonce attribute off <script> after applying
          // CSP, so the hydrated DOM shows nonce="" while React's tree carries
          // the real value — a benign, expected server/client diff. Suppress
          // the hydration warning for this element (same rationale as the
          // <html> suppressHydrationWarning above).
          suppressHydrationWarning
          dangerouslySetInnerHTML={{ __html: themeBootScript }}
        />
        {/*
         * JSON-LD structured data (mc#1486). Two graph nodes:
         *
         *   - Organization: surfaces the brand to Google Knowledge
         *     Graph + Bing entity index. URL+logo+sameAs are the
         *     minimum recommended set for new brands without a
         *     Wikipedia page.
         *
         *   - WebSite: enables the sitelinks search box and tells
         *     crawlers the canonical site URL when the same content
         *     is reachable via multiple subdomains (apex + tenant).
         *
         * Type-application/ld+json runs synchronously without
         * executing JS, so 'strict-dynamic' isn't required — we still
         * carry the nonce because production CSP's default-src 'self'
         * applies to any <script> element. The "type" attribute is
         * what keeps the browser from running the body as JS, but
         * CSP nonces are gated on the element not the type, so we
         * include the nonce too.
         */}
        <script
          type="application/ld+json"
          nonce={nonce}
          suppressHydrationWarning
          dangerouslySetInnerHTML={{
            __html: JSON.stringify({
              "@context": "https://schema.org",
              "@graph": [
                {
                  "@type": "Organization",
                  "@id": `${SITE_URL}#organization`,
                  name: "Enter OS",
                  url: SITE_URL,
                  logo: `${SITE_URL}/molecule-icon.png`,
                  sameAs: [
                    "https://git.moleculesai.app/molecule-ai",
                    "https://x.com/moleculeai",
                  ],
                },
                {
                  "@type": "WebSite",
                  "@id": `${SITE_URL}#website`,
                  url: SITE_URL,
                  name: "Enter OS",
                  publisher: { "@id": `${SITE_URL}#organization` },
                  inLanguage: "en-US",
                },
                {
                  "@type": "SoftwareApplication",
                  "@id": `${SITE_URL}#software`,
                  name: "Enter OS",
                  applicationCategory: "DeveloperApplication",
                  operatingSystem: "Web",
                  description:
                    "Org-chart canvas for AI agent teams with credit metering, audit, and one-click runtime provisioning.",
                  url: SITE_URL,
                  offers: {
                    "@type": "AggregateOffer",
                    priceCurrency: "USD",
                    lowPrice: "0",
                    highPrice: "99",
                    offerCount: "3",
                    url: `${SITE_URL}/pricing`,
                  },
                  publisher: { "@id": `${SITE_URL}#organization` },
                },
              ],
            }),
          }}
        />
      </head>
      <body className={`bg-surface text-ink ${displayFont.variable} ${sansFont.variable} ${monoFont.variable}`}>
        <ThemeProvider initialTheme={theme}>
          {/* ErrorBoundary is a client component; it catches render crashes
              anywhere inside AuthGate / children so a single failing view
              degrades to a reloadable fallback instead of a blank white screen. */}
          <ErrorBoundary>
            <AuthGate>{children}</AuthGate>
          </ErrorBoundary>
          <CookieConsent />
          {/* Demo Mock #1: post-purchase success toast. Mounted at the
              layout level so it persists across page state transitions
              (loading → hydrated → error) without being unmounted and
              losing its open-state. Reads ?purchase_success=1 from the
              URL on first paint, then strips the param. */}
          <PurchaseSuccessModal />
        </ThemeProvider>
      </body>
    </html>
  );
}
