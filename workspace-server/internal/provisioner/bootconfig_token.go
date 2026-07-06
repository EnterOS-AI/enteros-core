package provisioner

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Core-served boot-config token fetch (the FINAL, platform-agnostic config
// delivery — replaces both the R2 config relay and the local-only /configs
// volume mount).
//
// THE MODEL (endpoints matter):
//   - CONFIG comes from CORE (the tenant workspace-server), reachable by the
//     runtime container at PLATFORM_URL — http://<tenant>:8080 on the shared
//     local-docker network, or https://<slug>.moleculesai.app remotely. NEVER
//     the CP platform API (api.moleculesai.app) — a workspace fetching config
//     from the CP would be a control-plane dependency (core-oss-no-cp violation).
//   - MECHANISM: at (re)provision the tenant renders the config bundle ONCE
//     (collectCPConfigFiles), persists it host-side (PersistConfigBundleHostSide),
//     and MINTS a tiny one-time boot token. The token is injected as the env var
//     MOLECULE_CONFIG_BOOT_TOKEN into the provision request's Env map, which the
//     CP forwards verbatim into the runtime container (no CP config code — the CP
//     is a dumb transport for one opaque env var). At boot the container GETs
//     ${PLATFORM_URL}/internal/workspaces/boot-config with the token; the
//     tenant-server serves the rendered bundle ONCE and invalidates the token.
//   - No R2, no presigned URL, no size ceiling, no shared host volume — ONE path
//     for local + remote. The R2 config relay stays behind its flag, DORMANT for
//     config (kept only for future scale/NAT scenarios + the plugin channel).
//
// This file is the token half: a process-local, single-use, TTL-bounded token
// store. It imports only the std lib (crypto/rand, os, filepath, sync, time) —
// no CP, no cloud SDK, no R2 (core-OSS-clean). The store WRITER is CPProvisioner
// (mints at provision); the READER is the boot-config handler (redeems at fetch).
// main.go creates ONE store and hands the SAME instance to both so they cannot
// drift — identical to the hostStateDir wiring.

// BootConfigTokenTTL is the default lifetime of a minted boot token. A container
// fetches its config within the first seconds of boot, so a generous few-minute
// window comfortably covers a slow image pull + cold start while still bounding
// the exposure of an unredeemed token. Overridable via MOLECULE_BOOT_CONFIG_TTL
// (a Go duration string, e.g. "15m").
const BootConfigTokenTTL = 15 * time.Minute

type bootTokenEntry struct {
	workspaceID string
	expiresAt   time.Time
}

// BootConfigTokenStore is an in-process, single-use, TTL-bounded map of boot
// token -> workspace id. It is safe for concurrent use. It deliberately holds
// only {token -> workspaceID, expiry} — NOT the config bytes — so the config
// SSOT stays the host-side mirror on disk (one render, read fresh at serve).
type BootConfigTokenStore struct {
	mu      sync.Mutex
	entries map[string]bootTokenEntry
	ttl     time.Duration
	now     func() time.Time // injectable clock for tests
}

// NewBootConfigTokenStore constructs a store with the given TTL (<=0 → default).
func NewBootConfigTokenStore(ttl time.Duration) *BootConfigTokenStore {
	if ttl <= 0 {
		ttl = BootConfigTokenTTL
	}
	return &BootConfigTokenStore{
		entries: make(map[string]bootTokenEntry),
		ttl:     ttl,
		now:     time.Now,
	}
}

// Issue mints a fresh high-entropy boot token bound to workspaceID and returns
// it. Any previously-issued token for the same workspace is superseded (a Save &
// Restart reprovision invalidates the old token so a stale one can't be replayed
// against the new config). Opportunistically GCs expired entries.
func (s *BootConfigTokenStore) Issue(workspaceID string) (string, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return "", fmt.Errorf("bootconfig: empty workspace id")
	}
	raw := make([]byte, 32) // 256 bits
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("bootconfig: read random: %w", err)
	}
	token := hex.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	// Supersede any prior token for this workspace (single live token per ws).
	for t, e := range s.entries {
		if e.workspaceID == workspaceID {
			delete(s.entries, t)
		}
	}
	s.entries[token] = bootTokenEntry{workspaceID: workspaceID, expiresAt: s.now().Add(s.ttl)}
	return token, nil
}

// Lookup resolves a token to its workspace id WITHOUT consuming it, so a caller
// can validate + read the bundle and only Consume once the read succeeds (a
// transient server-side read failure then leaves the token replayable for the
// container's retry). Returns ("", false) for an unknown or expired token.
func (s *BootConfigTokenStore) Lookup(token string) (string, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[token]
	if !ok {
		return "", false
	}
	if !s.now().Before(e.expiresAt) {
		delete(s.entries, token)
		return "", false
	}
	return e.workspaceID, true
}

// Consume permanently invalidates a token (single-use). Idempotent.
func (s *BootConfigTokenStore) Consume(token string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, token)
}

func (s *BootConfigTokenStore) gcLocked() {
	now := s.now()
	for t, e := range s.entries {
		if !now.Before(e.expiresAt) {
			delete(s.entries, t)
		}
	}
}

// BuildConfigBundleJSON reads the host-side /configs mirror for a workspace and
// returns the rendered bundle as a {relpath: base64(content)} map — the EXACT
// wire shape the runtime's config unpack expects (identical to the R2 relay
// bundle), so the runtime's fetch/unpack is transport-agnostic. Returns an empty
// map (not an error) when the mirror dir is absent; returns an error only on a
// genuine read failure. Skips symlinks (OFFSEC-010) and non-regular files.
func BuildConfigBundleJSON(mirrorDir string) (map[string]string, error) {
	out := make(map[string]string)
	if strings.TrimSpace(mirrorDir) == "" {
		return out, nil
	}
	info, err := os.Stat(mirrorDir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return out, nil
	}
	walkErr := filepath.Walk(mirrorDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == mirrorDir {
			return nil
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			// Do not follow symlinks out of the mirror.
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if fi.IsDir() || !fi.Mode().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(mirrorDir, path)
		if rerr != nil {
			return rerr
		}
		data, derr := os.ReadFile(path)
		if derr != nil {
			return derr
		}
		out[filepath.ToSlash(rel)] = base64.StdEncoding.EncodeToString(data)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return out, nil
}
