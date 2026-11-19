package handlers

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// rewriteForDocker selects the same route for chat and restart signals.
// Only managed containers have a Docker DNS name. A failed runtime lookup
// must not turn an external agent's address into a guessed container route.
func (h *WorkspaceHandler) rewriteForDocker(ctx context.Context, agentURL, workspaceID string) (string, error) {
	if !platformInDocker || h.provisioner == nil || !isLoopbackHTTPURL(agentURL) {
		return agentURL, nil
	}
	var runtime string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(runtime, 'claude-code') FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&runtime); err != nil {
		return "", fmt.Errorf("read workspace runtime: %w", err)
	}
	if isExternalLikeRuntime(runtime) {
		return agentURL, nil
	}
	return provisioner.InternalURL(workspaceID), nil
}

// Parse the hostname so localhost, IPv4 and IPv6 loopback work without
// matching lookalike external hostnames or rewriting credential-bearing URLs.
func isLoopbackHTTPURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" || u.User != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
