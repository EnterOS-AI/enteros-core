// Package wiring constructs the v2 memory plugin dependency bundle
// at boot time so handlers can opt into the plugin path uniformly.
//
// The bundle is nil-safe: when MEMORY_PLUGIN_URL is unset, Build
// returns (nil, nil) so callers can detect "v2 not configured" with
// a single nil check instead of plumbing a feature flag through
// every handler.
//
// This package exists because the v2 plugin client + namespace
// resolver are needed by THREE different handler types (MCPHandler,
// AdminMemoriesHandler, WorkspaceHandler) constructed in two
// different files (main.go for WorkspaceHandler, router.go for the
// other two). A central Build() avoids each construction site
// re-implementing the env-var read + plugin instantiation.
package wiring

import (
	"context"
	"database/sql"
	"log"
	"os"
	"time"

	mclient "github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/client"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/namespace"
)

// Bundle is the v2 dependency bundle. Pass it through Setup as a
// single param; handlers extract what they need.
//
// nil receiver = "v2 not configured" — every method on Bundle
// nil-checks itself, so callers can pass a nil Bundle through the
// hot path without conditional spread.
type Bundle struct {
	Plugin   *mclient.Client
	Resolver *namespace.Resolver
}

// Build returns a wired Bundle if MEMORY_PLUGIN_URL is set, else nil.
//
// It probes /v1/health at boot — when the plugin is unreachable, we
// log a warning but STILL return the bundle. The MCP layer's
// circuit breaker handles ongoing unavailability; we don't want to
// block workspace-server boot just because the memory plugin is
// briefly down.
//
// Silent-misconfig guard: if MEMORY_V2_CUTOVER=true is set without
// MEMORY_PLUGIN_URL, the cutoverActive() check in handlers silently
// returns false and the legacy SQL path serves every request. The
// operator sees no errors, no warnings, and assumes the cutover is
// live. Log a LOUD WARN at boot when the env is half-configured so
// the misconfig is visible in the boot log, not detectable only by
// observing that the legacy table is still being written to.
func Build(db *sql.DB) *Bundle {
	cutover := os.Getenv("MEMORY_V2_CUTOVER") == "true"
	pluginURL := os.Getenv("MEMORY_PLUGIN_URL")

	if pluginURL == "" {
		if cutover {
			log.Printf("memory-plugin: ⚠️  MEMORY_V2_CUTOVER=true but MEMORY_PLUGIN_URL is unset — cutover is INACTIVE, legacy SQL path is serving every request. Either unset MEMORY_V2_CUTOVER or point MEMORY_PLUGIN_URL at a reachable plugin server.")
		}
		return nil
	}
	plugin := mclient.New(mclient.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if hr, err := plugin.Boot(ctx); err != nil {
		// Log even louder when cutover is on — an unreachable plugin
		// during cutover means writes that the operator THINKS are
		// going to v2 will silently fall back to legacy via the
		// circuit breaker on each request. Make it impossible to miss.
		if cutover {
			log.Printf("memory-plugin: ⚠️  MEMORY_V2_CUTOVER=true and MEMORY_PLUGIN_URL=%s but /v1/health probe failed (%v). Cutover writes will fall back to legacy via circuit breaker. Verify the plugin server is reachable.", pluginURL, err)
		} else {
			log.Printf("memory-plugin: /v1/health probe failed (will retry per-request): %v", err)
		}
	} else {
		log.Printf("memory-plugin: ok, capabilities=%v", hr.Capabilities)
	}
	return &Bundle{
		Plugin:   plugin,
		Resolver: namespace.New(db),
	}
}

// NamespaceCleanupFn returns a closure suitable for
// WorkspaceHandler.WithNamespaceCleanup. nil when bundle is nil so
// callers can pass it through unconditionally.
//
// The closure runs best-effort: errors are logged, never propagated.
// A misbehaving plugin must not block workspace purges.
func (b *Bundle) NamespaceCleanupFn() func(context.Context, string) {
	if b == nil || b.Plugin == nil {
		return nil
	}
	return func(ctx context.Context, workspaceID string) {
		ns := "workspace:" + workspaceID
		if err := b.Plugin.DeleteNamespace(ctx, ns); err != nil {
			log.Printf("memory-plugin: namespace cleanup failed (workspace=%s ns=%s): %v",
				workspaceID, ns, err)
		}
	}
}
