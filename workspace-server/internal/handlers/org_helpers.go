package handlers

// org_helpers.go — utility functions for org template processing.
// Prompt resolution, env file parsing, category routing, plugin merging,
// path sanitization.

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/cronspec"
)

// resolvePromptRef reads a prompt body from either an inline string or a
// file ref relative to the workspace's files_dir. Inline always wins when
// both are non-empty (caller-provided inline is more authoritative than a
// file path that may not exist yet during dev loops).
//
// File resolution:
//   - `<orgBaseDir>/<filesDir>/<fileRef>` when filesDir is non-empty
//   - `<orgBaseDir>/<fileRef>` when filesDir is empty (defaults-level refs)
//
// Both paths go through resolveInsideRoot so a crafted fileRef can't escape
// the org template directory via traversal (same defense the files_dir
// copy-step uses).
//
// Returns (resolved body, error). If both inline and fileRef are empty,
// returns ("", nil) — caller decides whether that's a problem.
func resolvePromptRef(inline, fileRef, orgBaseDir, filesDir string) (string, error) {
	if inline != "" {
		return inline, nil
	}
	if fileRef == "" {
		return "", nil
	}
	if orgBaseDir == "" {
		// Inline-only template (POST /org/import with a raw Template in the
		// JSON body, not a dir). File refs can't be resolved — surface the
		// problem rather than silently returning empty.
		return "", fmt.Errorf("prompt_file %q requires a dir-based org template (no orgBaseDir in inline-template mode)", fileRef)
	}
	searchRoot := orgBaseDir
	if filesDir != "" {
		p, err := resolveInsideRoot(orgBaseDir, filesDir)
		if err != nil {
			return "", fmt.Errorf("invalid files_dir %q: %w", filesDir, err)
		}
		searchRoot = p
	}
	abs, err := resolveInsideRoot(searchRoot, fileRef)
	if err != nil {
		return "", fmt.Errorf("invalid prompt_file %q: %w", fileRef, err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("read prompt_file %q: %w", fileRef, err)
	}
	return string(data), nil
}

// envVarRefPattern matches actual ${VAR} or $VAR references (not literal $).
// Used to detect unresolved placeholders without false positives like "$5".
// Requires [a-zA-Z_] as the first char after $ so $100 stays literal.
// Two capture groups: (1) ${VAR} form, (2) $VAR form.
var envVarRefPattern = regexp.MustCompile(`\$\{([a-zA-Z_][a-zA-Z0-9_]*)\}|\$([a-zA-Z_][a-zA-Z0-9_]*)`)

// hasUnresolvedVarRef returns true if the original string had a ${VAR} or $VAR
// reference that the expanded string didn't fully replace (i.e. the var was unset).
func hasUnresolvedVarRef(original, expanded string) bool {
	if !envVarRefPattern.MatchString(original) {
		return false // no var refs to resolve
	}
	// If expansion produced the same string and that string still has refs, unresolved.
	// If expansion stripped them to "", also unresolved.
	return expanded == "" || envVarRefPattern.MatchString(expanded)
}

// expandWithEnv expands ${VAR} and $VAR references in s using the env map.
// Falls back to the platform process env only when the whole value is a
// single variable reference; embedded process-env expansion is too broad for
// imported org YAML because host variables such as HOME are not template data.
func expandWithEnv(s string, env map[string]string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}

		if i+1 >= len(s) {
			b.WriteByte('$')
			i++
			continue
		}

		if s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				b.WriteByte('$')
				i++
				continue
			}
			end += i + 2
			key := s[i+2 : end]
			ref := s[i : end+1]
			b.WriteString(expandEnvRef(key, ref, s, env))
			i = end + 1
			continue
		}

		if !isEnvIdentStart(s[i+1]) {
			b.WriteByte('$')
			i++
			continue
		}
		j := i + 2
		for j < len(s) && isEnvIdentPart(s[j]) {
			j++
		}
		key := s[i+1 : j]
		ref := s[i:j]
		b.WriteString(expandEnvRef(key, ref, s, env))
		i = j
	}
	return b.String()
}

// expandEnvRef resolves a single variable reference extracted from s.
//
// Guards:
//   - Empty key → "$$" escape, return "$"
//   - key[0] not POSIX ident start → "$" + partial chars, return "$<chars>"
//   - Key in env map → return the mapped value (template override wins)
//   - Otherwise → only fall back to os.Getenv if the whole input string IS the
//     variable reference (ref == whole).
//
// Bare $VAR format:
//
//	$HOME (alone) → ref==whole → os.Getenv ✓  (host HOME is org-template HOME)
//	$HOME/path (partial) → ref!=whole → literal "$HOME" ✓  (CWE-78: prevents host leak)
//
// Braced ${VAR} format:
//
//	${HOME} (alone) → ref==whole → os.Getenv ✓
//	${ROLE}/admin (partial) → ref!=whole → literal ✓
//	"yes and ${NOT_SET}" (embedded) → ref!=whole → literal ✓
//
// This is the CWE-78 fix from commit a3a358f9.
func expandEnvRef(key, ref, whole string, env map[string]string) string {
	if key == "" {
		return "$"
	}
	if !isEnvIdentStart(key[0]) {
		return "$" + key
	}
	if v, ok := env[key]; ok {
		return v
	}
	if ref == whole {
		return os.Getenv(key)
	}
	return ref
}

func isEnvIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isEnvIdentPart(c byte) bool {
	return isEnvIdentStart(c) || (c >= '0' && c <= '9')
}

// loadWorkspaceEnv reads the org root .env and the workspace-specific .env files.
// (workspace overrides org root). Used by both secret injection and channel
// config expansion.
//
// SECURITY: filesDir is sourced from untrusted org YAML input (ws.FilesDir).
// resolveInsideRoot guard prevents path traversal (CWE-22) where a malicious
// filesDir like "../../../etc" could escape the org root.
func loadWorkspaceEnv(orgBaseDir, filesDir string) map[string]string {
	envVars := map[string]string{}
	if orgBaseDir == "" {
		return envVars
	}
	parseEnvFile(filepath.Join(orgBaseDir, ".env"), envVars)
	if filesDir != "" {
		safeFilesDir, err := resolveInsideRoot(orgBaseDir, filesDir)
		if err != nil {
			// Reject traversal attempt silently — callers expect an empty map
			// on any read failure.
			log.Printf("loadWorkspaceEnv: rejecting filesDir %q: %v", filesDir, err)
			return envVars
		}
		parseEnvFile(filepath.Join(safeFilesDir, ".env"), envVars)
	}
	return envVars
}

// loadPersonaEnvFile merges per-role persona credentials into out. The file
// lives at $MOLECULE_PERSONA_ROOT/<role>/env (default
// /etc/molecule-bootstrap/personas). A deployment may populate one directory
// per dev-tree role, each carrying the role's
// Gitea identity (GITEA_USER, GITEA_TOKEN, GITEA_TOKEN_SCOPES,
// GITEA_USER_EMAIL, GITEA_SSH_KEY_PATH).
//
// Lower precedence than the org and workspace .env files: callers should
// invoke this BEFORE parseEnvFile on those, so a workspace .env can
// override a persona-default value when needed.
//
// Silent no-op when role is empty, when the role name fails the safe-segment
// check, or when the env file does not exist (workspaces without a role —
// or running on hosts that don't ship the bootstrap dir — keep their old
// behavior).
//
// Token-file fallback: the newer prod-team personas (agent-dev-a,
// agent-dev-b, agent-pm) ship `token` + `universal-auth.env` only — no
// legacy plaintext `env` file. When the env-file load produces zero rows,
// loadPersonaTokenFile fills in GITEA_TOKEN / GITEA_USER / GITEA_USER_EMAIL
// from the token file so the GIT_ASKPASS helper has something to emit.
// The env-file form remains authoritative when present (it may carry
// richer rows like GITEA_TOKEN_SCOPES / GITEA_SSH_KEY_PATH).
func loadPersonaEnvFile(role string, out map[string]string) {
	if !isSafeRoleName(role) {
		if role != "" {
			log.Printf("Org import: refusing persona env load for unsafe role name %q", role)
		}
		return
	}
	root := os.Getenv("MOLECULE_PERSONA_ROOT")
	if root == "" {
		root = "/etc/molecule-bootstrap/personas"
	}
	before := len(out)
	parseEnvFile(filepath.Join(root, role, "env"), out)
	if len(out) == before {
		// No env-file rows landed (file absent, or present-but-empty).
		// Try the token-only persona shape used by the prod-team
		// identities. Existing keys in out are preserved.
		loadPersonaTokenFile(role, out)
	}
}

// loadPersonaTokenFile populates GITEA_TOKEN / GITEA_USER / GITEA_USER_EMAIL
// from a persona dir that ships only the bare `token` file — the shape used
// by the production agent personas (agent-dev-a, agent-dev-b, agent-pm).
// Those dirs do not carry an `env` file because their non-Gitea creds come
// from Infisical Universal Auth at runtime (universal-auth.env), so the
// historical loadPersonaEnvFile path silently no-ops on them.
//
// File layout: $MOLECULE_PERSONA_ROOT/<role>/token (mode 600, plain text).
// The token contents become GITEA_TOKEN (whitespace-trimmed); the role
// name becomes GITEA_USER; GITEA_USER_EMAIL is synthesised as
// <role>@<gitIdentityEmailDomain> to match the email shape that
// applyAgentGitIdentity uses for its slug-derived authorship addresses.
//
// Silent no-op when the role fails the safe-segment check, when the
// token file does not exist, or when its contents are empty after
// trimming. Existing keys in out are not overwritten — the caller's
// later .env layers and any prior loadPersonaEnvFile rows always win.
func loadPersonaTokenFile(role string, out map[string]string) {
	if out == nil {
		return
	}
	if !isSafeRoleName(role) {
		return
	}
	root := os.Getenv("MOLECULE_PERSONA_ROOT")
	if root == "" {
		root = "/etc/molecule-bootstrap/personas"
	}
	data, err := os.ReadFile(filepath.Join(root, role, "token"))
	if err != nil {
		return
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return
	}
	if _, ok := out["GITEA_TOKEN"]; !ok {
		out["GITEA_TOKEN"] = token
	}
	if _, ok := out["GITEA_USER"]; !ok {
		out["GITEA_USER"] = role
	}
	if _, ok := out["GITEA_USER_EMAIL"]; !ok {
		out["GITEA_USER_EMAIL"] = role + "@" + gitIdentityEmailDomain
	}
}

// isSafeRoleName accepts a single path segment of [A-Za-z0-9_-]+. Rejects
// empty, ".", "..", and anything containing a path separator — even though
// the construct is admin-only, defense-in-depth keeps the persona dir
// shape invariant: one flat directory per role, no climbing out.
func isSafeRoleName(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

// parseEnvFile reads a .env file and adds KEY=VALUE pairs to the map.
// Skips comments (#) and empty lines. Values can be quoted.
func parseEnvFile(path string, out map[string]string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		// Strip surrounding quotes
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		if key != "" && value != "" {
			out[key] = value
		}
	}
}

// mergeCategoryRouting unions defaults.category_routing with per-workspace
// category_routing. Workspace-level keys override the default's value for that
// key (the role list is replaced wholesale, not unioned per-key, so a workspace
// can narrow a category — e.g. "infra: [DevOps Only]"). Empty role lists drop
// the category entirely. See issue #51.
func mergeCategoryRouting(defaultRouting, wsRouting map[string][]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range defaultRouting {
		if k == "" || len(v) == 0 {
			continue
		}
		cp := make([]string, len(v))
		copy(cp, v)
		out[k] = cp
	}
	for k, v := range wsRouting {
		if k == "" {
			continue
		}
		if len(v) == 0 {
			// Empty list = explicit "drop this category for this workspace"
			delete(out, k)
			continue
		}
		cp := make([]string, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// renderCategoryRoutingYAML emits a deterministic YAML block of the form:
//
//	category_routing:
//	  security: [Backend Engineer, DevOps]
//	  ui: [Frontend Engineer]
//
// Keys are sorted for stable, test-friendly output. Uses yaml.Node + yaml.Marshal
// so role names containing YAML-reserved characters (colons, quotes, unicode line
// separators, etc.) are escaped by the YAML library — no ad-hoc quoting.
func renderCategoryRoutingYAML(routing map[string][]string) (string, error) {
	if len(routing) == 0 {
		return "", nil
	}
	keys := make([]string, 0, len(routing))
	for k := range routing {
		if k == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	inner := &yaml.Node{Kind: yaml.MappingNode}
	for _, k := range keys {
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: k}
		valNode := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for _, role := range routing[k] {
			valNode.Content = append(valNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: role})
		}
		inner.Content = append(inner.Content, keyNode, valNode)
	}
	doc := &yaml.Node{Kind: yaml.MappingNode}
	doc.Content = []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "category_routing"},
		inner,
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// appendYAMLBlock concatenates a YAML fragment to an existing buffer, guaranteeing
// a newline boundary between them. Upstream code writes config.yaml in fragments
// (base template → category_routing → initial_prompt) and the base isn't
// guaranteed to end in \n, which would merge the last line into the next block.
func appendYAMLBlock(existing []byte, block string) []byte {
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		existing = append(existing, '\n')
	}
	return append(existing, []byte(block)...)
}

// renderedTemplateSchedule is one entry of the delivered `schedules:` block.
// The key set mirrors EXACTLY what the runtime's config.yaml seeding accepts
// (molecule-ai-workspace-runtime schedule_seed._normalize_config_entry →
// validate_entry filters to name/cron/timezone/prompt/enabled): the prompt is
// always inlined — never a prompt_file ref, which would dangle in-container —
// and `cron` is the runtime-native key (core's `cron_expr` authoring alias is
// resolved here, not shipped).
//
// Prompt is a yaml.Node (built by schedulePromptNode) rather than a plain
// string: the yaml.v3 emitter's block-scalar form is BROKEN for a multi-line
// string whose first line starts with whitespace — it emits an explicit
// indentation indicator (`|4-`) with content indentation that neither yaml.v3
// itself nor PyYAML can re-parse. Delivered as-is that would make the WHOLE
// config.yaml unloadable: runtime config.py load_config has no try/except →
// workspace boot bricks, and schedule_seed's YAMLError catch drops every
// schedule including valid siblings (PR #4444 review CRITICAL). The node lets
// us force a portable style for exactly that class.
type renderedTemplateSchedule struct {
	Name     string    `yaml:"name"`
	Cron     string    `yaml:"cron"`
	Timezone string    `yaml:"timezone"`
	Prompt   yaml.Node `yaml:"prompt"`
	Enabled  bool      `yaml:"enabled"`
}

// reparsedTemplateSchedule is the plain-string readback shape used only by
// the round-trip guard in renderTemplateSchedulesYAML — keep it
// field-for-field equal to renderedTemplateSchedule.
type reparsedTemplateSchedule struct {
	Name     string `yaml:"name"`
	Cron     string `yaml:"cron"`
	Timezone string `yaml:"timezone"`
	Prompt   string `yaml:"prompt"`
	Enabled  bool   `yaml:"enabled"`
}

// renderedScheduleDoc is the marshal/unmarshal envelope for the block.
type renderedScheduleDoc struct {
	Schedules []renderedTemplateSchedule `yaml:"schedules"`
}

// schedulePromptNode wraps a prompt body for emission, normalizing the shape
// the yaml.v3 emitter mis-encodes (see renderedTemplateSchedule): a
// multi-line prompt whose FIRST line is empty or starts with space/tab —
// benign, realistic content like an indented "  Please review:" opener or a
// code fence — is forced to DoubleQuotedStyle, which keeps the content
// byte-identical while emitting fully portable escaped-string YAML
// (empirically round-trips under both yaml.v3 and PyYAML 6.0.3). Every other
// prompt keeps the emitter's default, readable block-scalar style.
func schedulePromptNode(prompt string) yaml.Node {
	n := yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: prompt}
	firstLine, _, _ := strings.Cut(prompt, "\n")
	if strings.Contains(prompt, "\n") && (firstLine == "" || firstLine != strings.TrimLeft(firstLine, " \t")) {
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}

// scheduleNamePattern mirrors the runtime schedule contract's name rule
// EXACTLY (molecule-ai-workspace-runtime molecule_runtime/contracts/
// schedule.schema.json, $defs.scheduleEntry.name: pattern
// ^[a-z0-9]+(?:-[a-z0-9]+)*$, minLength 1, maxLength 128; parity pinned by
// TestScheduleRenderContract_MatchesRuntimeSchema). A name that fails it
// would render core-side but be silently SKIPPED by the runtime's
// validate-and-skip seeding — split-brain vs the legacy DB grid today, and
// silent loss once P4b retires the DB leg — so core skips it LOUDLY at
// import time instead.
var scheduleNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// maxScheduleNameLen mirrors the runtime schema's name maxLength (the name
// grammar is ASCII, so Go byte length == the schema's code-point length).
const maxScheduleNameLen = 128

// scheduleEntryRoundTrips is the per-entry emitter guard: it marshals the
// single entry as its own schedules doc, re-parses it, and reports whether
// every field survives byte-exact. Any residual yaml.v3 emitter
// incompatibility (beyond the first-line-indent class schedulePromptNode
// normalizes away) is caught HERE and the entry is skipped by the caller —
// never shipped to brick the delivered config.yaml.
func scheduleEntryRoundTrips(entry renderedTemplateSchedule) bool {
	out, err := yaml.Marshal(renderedScheduleDoc{Schedules: []renderedTemplateSchedule{entry}})
	if err != nil {
		return false
	}
	var back struct {
		Schedules []reparsedTemplateSchedule `yaml:"schedules"`
	}
	if err := yaml.Unmarshal(out, &back); err != nil || len(back.Schedules) != 1 {
		return false
	}
	got := back.Schedules[0]
	return got.Name == entry.Name && got.Cron == entry.Cron &&
		got.Timezone == entry.Timezone && got.Prompt == entry.Prompt.Value &&
		got.Enabled == entry.Enabled
}

// renderTemplateSchedulesYAML renders a workspace's RESOLVED template/org
// schedules as a top-level `schedules:` YAML block for the DELIVERED
// config.yaml — the scheduler-as-trigger-plugin RFC §8A P3 template-seeding
// seam, core leg (mechanism decided on issue #4411, 2026-07-17: schedules
// travel over the workspace config/asset channel; the runtime's
// seed_schedules_from_workspace_config, molecule-ai-workspace-runtime#318,
// reconciles the block into the volume-authoritative grid at boot AND on
// POST /internal/daemons/reload with ScheduleStore.upsert_template
// source=template semantics — runtime edits survive, tombstones respected).
//
// Prompt bodies are INLINED: a `prompt_file:` ref resolves HOST-SIDE against
// the org template dir (orgBaseDir / filesDir, via resolvePromptRef) which
// does not exist inside the container, so shipping the ref would always
// dangle. Timezone defaults to "UTC" (same as the legacy DB seed).
//
// Per-entry validate-and-skip, mirroring the legacy DB seed loop and the
// runtime's own contract (caps MAX_ENTRIES=100 / MAX_CRON_LEN=128 /
// MAX_PROMPT_BYTES=16384 — kept in lockstep by maxTemplateSchedules /
// maxScheduleCronExprLen / maxSchedulePromptBytes — plus the lowercase-kebab
// name grammar, scheduleNamePattern): an entry with a non-contract name,
// over-long or unschedulable cron, unresolvable/empty/oversize prompt, or
// one that fails the emitter round-trip guard (scheduleEntryRoundTrips) is
// logged (name %q-quoted, CRLF-safe) and skipped so one bad entry never
// drops its valid siblings; entries beyond maxTemplateSchedules are skipped
// wholesale. Pre-validating here keeps a hostile org.yaml from shipping a
// block the runtime would reject wholesale (its MAX_ENTRIES overflow rejects
// the whole upsert atomically).
//
// Returns the YAML block ("" when nothing rendered), the rendered count, and
// the skipped count. Callers MUST NOT append an empty block — a template with
// no renderable schedules keeps the delivered config.yaml byte-identical.
func renderTemplateSchedulesYAML(schedules []OrgSchedule, orgBaseDir, filesDir, wsName string) (block string, rendered, skipped int) {
	if len(schedules) == 0 {
		return "", 0, 0
	}
	entries := make([]renderedTemplateSchedule, 0, len(schedules))
	for i, sched := range schedules {
		if len(entries) >= maxTemplateSchedules {
			remaining := len(schedules) - i
			log.Printf("Org import: schedule render for %s hit the %d-entry cap — skipping %d remaining entrie(s)", wsName, maxTemplateSchedules, remaining)
			skipped += remaining
			break
		}
		// Name must satisfy the runtime contract (scheduleNamePattern +
		// maxScheduleNameLen — see the var doc): a non-conforming name would
		// be silently dropped by the runtime's validate-and-skip seeding.
		if sched.Name == "" || len(sched.Name) > maxScheduleNameLen || !scheduleNamePattern.MatchString(sched.Name) {
			log.Printf("Org import: schedule render for %s: entry %d name %q violates the schedule contract (lowercase kebab ^[a-z0-9]+(?:-[a-z0-9]+)*$, ≤%d chars — the runtime would silently skip it) — skipping", wsName, i, sched.Name, maxScheduleNameLen)
			skipped++
			continue
		}
		if len(sched.CronExpr) > maxScheduleCronExprLen {
			log.Printf("Org import: schedule render: cron_expr too long (%d > %d) for %q on %s — skipping", len(sched.CronExpr), maxScheduleCronExprLen, sched.Name, wsName)
			skipped++
			continue
		}
		tz := sched.Timezone
		if tz == "" {
			tz = "UTC"
		}
		// Same schedulability check the DB seed + the runtime store apply
		// (shared cron contract, internal/cronspec) — an unschedulable cron is
		// skipped at render time so the operator sees the failure in the import
		// log instead of a silent runtime-side skip on a box they can't read.
		if _, cronErr := cronspec.ComputeNextRun(sched.CronExpr, tz, time.Now()); cronErr != nil {
			log.Printf("Org import: schedule render: invalid cron for %q on %s: %v — skipping", sched.Name, wsName, cronErr)
			skipped++
			continue
		}
		prompt, promptErr := resolvePromptRef(sched.Prompt, sched.PromptFile, orgBaseDir, filesDir)
		if promptErr != nil {
			log.Printf("Org import: schedule render: failed to resolve prompt for %q on %s: %v — skipping", sched.Name, wsName, promptErr)
			skipped++
			continue
		}
		if prompt == "" {
			log.Printf("Org import: schedule render: schedule %q on %s has empty prompt — skipping", sched.Name, wsName)
			skipped++
			continue
		}
		if len(prompt) > maxSchedulePromptBytes {
			log.Printf("Org import: schedule render: prompt too long (%d > %d bytes) for %q on %s — skipping", len(prompt), maxSchedulePromptBytes, sched.Name, wsName)
			skipped++
			continue
		}
		enabled := true
		if sched.Enabled != nil {
			enabled = *sched.Enabled
		}
		entry := renderedTemplateSchedule{
			Name:     sched.Name,
			Cron:     sched.CronExpr,
			Timezone: tz,
			Prompt:   schedulePromptNode(prompt),
			Enabled:  enabled,
		}
		// Emitter round-trip guard (PR #4444 review CRITICAL): any entry whose
		// emitted YAML does not survive a byte-exact re-parse is skipped — a
		// mis-emitted entry in the delivered config.yaml would make the WHOLE
		// file unloadable and brick workspace boot. schedulePromptNode already
		// normalizes the known first-line-indent class; this catches anything
		// residual, preserving the per-entry-skip invariant (siblings survive).
		if !scheduleEntryRoundTrips(entry) {
			log.Printf("Org import: schedule render: entry %q on %s does not survive a YAML emit/parse round-trip (emitter incompatibility) — skipping to protect the delivered config.yaml", sched.Name, wsName)
			skipped++
			continue
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return "", 0, skipped
	}
	// yaml.Marshal on the typed struct so prompt bodies with YAML-reserved
	// content (colons, quotes, multi-line text) are escaped by the library —
	// same rationale as renderCategoryRoutingYAML.
	out, err := yaml.Marshal(renderedScheduleDoc{Schedules: entries})
	if err != nil {
		// Should be unreachable for plain string fields; treat as all-skipped
		// rather than delivering a half-formed block.
		log.Printf("Org import: schedule render for %s failed to marshal: %v — block dropped", wsName, err)
		return "", 0, len(schedules)
	}
	// Whole-block guard: every entry round-tripped individually, but never
	// deliver a block the assembled-file parser could choke on. If the full
	// block fails to re-parse, drop it (boot with no volume-seeded schedules —
	// the legacy DB grid still exists until P4b) rather than brick the boot.
	var whole struct {
		Schedules []reparsedTemplateSchedule `yaml:"schedules"`
	}
	if err := yaml.Unmarshal(out, &whole); err != nil || len(whole.Schedules) != len(entries) {
		log.Printf("Org import: schedule render for %s: assembled block failed re-parse (err=%v, entries=%d/%d) — block DROPPED to protect workspace boot", wsName, err, len(whole.Schedules), len(entries))
		return "", 0, len(schedules)
	}
	return string(out), len(entries), skipped
}

// appendYAMLBlockChecked appends a rendered block to config bytes and then
// re-parses the ASSEMBLED document; if the combined document does not load, it
// returns the original bytes unchanged (ok=false) with a loud log. Belt and
// braces over the per-entry + whole-block guards in
// renderTemplateSchedulesYAML: whatever happens upstream, an unparseable
// config.yaml must NEVER ship — the runtime loads it without a try/except and
// an unloadable file bricks workspace boot.
func appendYAMLBlockChecked(existing []byte, block, blockLabel, wsName string) ([]byte, bool) {
	combined := appendYAMLBlock(existing, block)
	var probe map[string]interface{}
	if err := yaml.Unmarshal(combined, &probe); err != nil {
		log.Printf("Org import: assembled config.yaml for %s fails to parse after appending the %s block — delivering WITHOUT it (would brick workspace boot): %v", wsName, blockLabel, err)
		return existing, false
	}
	return combined, true
}

// mergePlugins returns the union of defaults and per-workspace plugin lists
// (deduplicated, defaults first). A per-workspace entry starting with "!" or
// "-" opts that plugin OUT of the union. See issue #68.
func mergePlugins(defaultPlugins, wsPlugins []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(defaultPlugins)+len(wsPlugins))
	for _, p := range defaultPlugins {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, p := range wsPlugins {
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "!") || strings.HasPrefix(p, "-") {
			target := strings.TrimLeft(p, "!-")
			if target == "" {
				continue
			}
			if seen[target] {
				delete(seen, target)
				filtered := out[:0]
				for _, existing := range out {
					if existing != target {
						filtered = append(filtered, existing)
					}
				}
				out = filtered
			}
			continue
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// resolveInsideRoot joins `userPath` onto `root` and ensures the lexically
// cleaned result stays inside root. Rejects absolute paths outright and
// anything containing ".." that would escape the root.
//
// Both arguments are resolved to absolute paths via filepath.Abs before the
// prefix check so a root passed as a relative path still works correctly.
// Follows Go's standard pattern for SSRF-class path sanitization; using
// strings.HasPrefix on an absolute-path pair plus the separator guard rejects
// sibling directories that share a prefix (e.g. "/foo" vs "/foobar").
func resolveInsideRoot(root, userPath string) (string, error) {
	if userPath == "" {
		return "", fmt.Errorf("path is empty")
	}
	if filepath.IsAbs(userPath) {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("root abs: %w", err)
	}
	joined := filepath.Join(absRoot, userPath)
	// filepath.Join preserves "." components when root is absolute; clean
	// them before computing the final absolute path so "./subdir/./file.txt"
	// resolves to root/subdir/file.txt (not root/./subdir/./file.txt).
	cleaned := filepath.Clean(joined)
	absJoined, err := filepath.Abs(cleaned)
	if err != nil {
		return "", fmt.Errorf("joined abs: %w", err)
	}
	// Allow exact-root match (rare but valid) and any descendant.
	if absJoined != absRoot && !strings.HasPrefix(absJoined, absRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes root")
	}
	return absJoined, nil
}
