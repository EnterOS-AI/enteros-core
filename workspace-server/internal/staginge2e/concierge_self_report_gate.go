package staginge2e

// concierge_self_report_gate.go — Guard B failure mode G9: the concierge REPORTS
// SUCCESS for a workspace it never created.
//
// WHY THIS FILE EXISTS
// --------------------
//
// Guard B's callable proof (platform_agent_mgmt_mcp_gate.go check 5) asserts a
// deterministic side effect: after a real A2A `message/send` turn asking the
// concierge to run provision_workspace, a kind='workspace' row with the exact
// requested name must appear. That is the right primary evidence, and it is why
// Guard B is the only gate in the fleet that can catch G9 at all.
//
// But it reads ONLY the row. The concierge's own answer — the thing a user
// reads in the canvas, the thing a workflow keys on, the thing a health probe
// would scrape — is logged and then thrown away. Two consequences:
//
//  1. When the agent says "Done. The workspace has been created" and no row
//     exists, Guard B does go red — but it reports the generic
//     "not genuinely CALLABLE", indistinguishable from an agent that honestly
//     answered "I don't have that tool". Those need different fixes, and the
//     red never said which one happened.
//
//  2. When the agent says "Workspace ID: <uuid>" and the uuid is NOT the row it
//     actually created, Guard B is GREEN. The row exists under the requested
//     name, so check 5 is satisfied, and nothing ever compares the id the agent
//     PUBLISHED against the id that actually landed. A caller that trusts the
//     reported id — every downstream consumer, by construction — is holding a
//     handle to something that does not exist.
//
// The invariant this file adds is: A SELF-REPORT MUST NEVER BE THE EVIDENCE,
// and where a self-report exists it must be RECONCILED against the resource it
// claims, with a mismatch surfaced loudly rather than returned as success.
//
// Deliberately additive-only with respect to the row: nothing here can turn a
// MISSING row into a pass. The reconciliation can only ADD failures and NAME
// them. A missing row is still red via check 5 exactly as before.
//
// This file is UNTAGGED (no `staging_e2e`) for the same reason
// platform_agent_mgmt_mcp_gate.go is: the decision is pure data → verdict, so
// it is proved in the normal `go test ./...` gate against verbatim reply bodies
// harvested from real red and green Guard B runs — no live tenant needed.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ConciergeClaim is what the concierge ITSELF said about the provision turn,
// parsed from its actual reply body (not from the transport ack).
type ConciergeClaim struct {
	// Observed is true when at least one REAL agent reply text was retrieved.
	// A transport acknowledgement ({"queued":true,...} / {"status":"queued"})
	// is NOT a reply: it is the tenant saying "I took your message", which is
	// precisely the 202-treated-as-done shape this gate must not score. When
	// Observed is false the reconciliation abstains — the row stays the
	// evidence — and the verdict SAYS SO rather than implying the report was
	// checked.
	Observed bool

	// Texts are the verbatim reply texts, in the order retrieved. Kept so a red
	// verdict can quote the exact sentence that lied.
	Texts []string

	// ClaimsCreated is true when at least one reply ASSERTS the workspace was
	// created/provisioned. See ReplyClaimsWorkspaceCreated for what does and
	// does not count — an honest refusal, a runtime error string and an empty
	// body must all be false, or this gate degrades into `grep -q ""`.
	ClaimsCreated bool

	// ClaimedWorkspaceIDs are the ids the reply PUBLISHES as the new workspace's
	// id (explicitly labelled, or a reply that is nothing but a bare uuid).
	// Unlabelled uuids are deliberately NOT collected: a reply routinely names
	// the org id or the concierge's own id, and treating those as an identity
	// claim would false-fail a hard prod gate.
	ClaimedWorkspaceIDs []string
}

// ── Reply extraction ────────────────────────────────────────────────────────

// a2aReplyEnvelope covers the three shapes Guard B can hold a reply in:
//
//  1. the synchronous JSON-RPC result of POST /workspaces/:id/a2a
//     {"jsonrpc":"2.0","result":{"kind":"message","parts":[{"kind":"text",...}]}}
//  2. the queue-status projection of GET /workspaces/:id/a2a/queue/:queue_id
//     {"queue_id":...,"status":"completed","response_body":<shape 1>}
//  3. the transport ack, which carries NO reply
//     {"queued":true,"queue_id":...} / {"status":"queued"} / {"delivery_mode":...}
//
// Only the two fields that can CARRY an utterance are decoded. Shape 3 is
// recognised by the absence of both, which is deliberately the default: a body
// we do not understand is "no reply observed", never "the agent said nothing
// wrong".
type a2aReplyEnvelope struct {
	Result       *a2aMessageResult `json:"result"`
	ResponseBody json.RawMessage   `json:"response_body"`
}

type a2aMessageResult struct {
	Parts []struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
	} `json:"parts"`
}

// ConciergeReplyText extracts the agent's reply text from one raw body.
//
// isReply=false means "this body carried no agent utterance" — a transport ack,
// an in-flight queue row, an empty body, or an unparseable one. The caller must
// treat that as NOT OBSERVED, never as "the agent said nothing wrong".
func ConciergeReplyText(body string) (text string, isReply bool) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", false
	}
	var env a2aReplyEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return "", false
	}
	// Shape 3 first: an ack is never a reply, even if some future field on it
	// happens to decode.
	if env.Result == nil && len(env.ResponseBody) == 0 {
		return "", false
	}
	// Shape 2: recurse into the queue row's stored response body.
	if env.Result == nil {
		return ConciergeReplyText(string(env.ResponseBody))
	}
	// Shape 1.
	var sb strings.Builder
	for _, p := range env.Result.Parts {
		if p.Kind != "" && p.Kind != "text" {
			continue
		}
		if strings.TrimSpace(p.Text) == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(p.Text)
	}
	if sb.Len() == 0 {
		// A result envelope with no text part is a reply we cannot read; do not
		// pretend we read one.
		return "", false
	}
	return sb.String(), true
}

// QueuedA2AQueueID returns the queue id from a transport ACK, or "" when the
// body is not an ack that carries one.
//
// This is the hinge that keeps the G9 check from being a phantom gate. The
// modern tenant answers POST /workspaces/:id/a2a with
// {"queued":true,"queue_id":...} and delivers the agent's real answer later, so
// on today's staging fleet the POST response contains NO self-report at all.
// Without following the queue id to GET /workspaces/:id/a2a/queue/:queue_id
// there would be nothing to reconcile and the check would be permanently inert.
func QueuedA2AQueueID(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	var env struct {
		QueueID string `json:"queue_id"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return ""
	}
	id := strings.TrimSpace(env.QueueID)
	// VALIDATE AT THE SOURCE. This id is the ONLY caller-supplied component of
	// the queue-status URL, and it arrives from the tenant's JSON. Round-2
	// review found that doTenantJSONTimeout still has two t.Fatalf paths — a
	// URL that fails tenantTopoFromURL or http.NewRequest — and http.NewRequest
	// does reject a control character in a URL. So "the diagnostic read can
	// never fail the test on its own" was true of the collector but not,
	// literally, of the request helper underneath it.
	//
	// Rejecting anything that is not a plain token closes that at the point of
	// entry rather than restating the claim more carefully: a control char,
	// whitespace, a slash (which would also re-route the request to a different
	// endpoint) or an absurd length yields "", the id is never followed, and
	// the collector simply observes nothing.
	if !safeQueueIDRe.MatchString(id) || strings.Contains(id, "..") || !queueIDHasAlnumRe.MatchString(id) {
		return ""
	}
	return id
}

// queueIDHasAlnumRe requires an identifier to contain at least one
// alphanumeric. Without it "." and ".." and "--" are all legal under
// safeQueueIDRe's character class — they are PATH SEGMENTS, not ids, and a
// single "." re-routes a URL just as surely as a slash does.

// safeQueueIDRe admits only an opaque URL-path token: the queue ids the tenant
// actually mints are UUIDs, and this is deliberately a little wider than that
// without ever admitting a character that could break or redirect the request.
//
// The ".." and alphanumeric rejections above close the traversal that needs no
// slash (round-3/4 review): "." and ".." are legal under this character class
// but are path segments, not identifiers.
var safeQueueIDRe = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)

var queueIDHasAlnumRe = regexp.MustCompile(`[A-Za-z0-9]`)

// A2AQueueTerminalStatus reports whether a queue row has stopped moving, so the
// live poller knows when the agent's answer is as final as it will get. An
// unknown/blank status is NOT terminal — the poller keeps waiting rather than
// scoring an in-flight turn as "the agent said nothing".
func A2AQueueTerminalStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "failed", "dropped", "expired":
		return true
	}
	return false
}

// queueStatusOf reads the `status` field of a queue-status body. Anything
// unparseable is "" — which A2AQueueTerminalStatus treats as NOT terminal, so
// an unreadable body makes the poller keep waiting rather than record silence.
func queueStatusOf(body string) string {
	var env struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return ""
	}
	return env.Status
}

// SelfReportFetch reads one queue row. It returns the HTTP status and body in
// the shape doTenantJSONTimeout produces, where a TRANSPORT FAILURE is
// (0, "") — never a panic, never a fatal.
type SelfReportFetch func(queueID string) (httpStatus int, body string)

// CollectSelfReportBodies gathers the concierge's actual replies for a provision
// turn by following the queue ids the tenant handed back, and appends them to
// the bodies already in hand.
//
// It is the pure core of the live collector so the property the gate depends on
// is TESTABLE without a tenant: **this can only ever abstain.** There is no
// error return and no fatal path. A fetch that transport-fails, 404s, 403s, or
// never leaves an in-flight status contributes nothing; the result is simply
// fewer bodies, which makes ParseConciergeClaim report Observed=false and
// ReconcileProvisionClaim abstain. A diagnostic read must never be able to fail
// a deploy gate.
//
// budget is a SINGLE TOTAL wall-clock allowance shared across every queue id,
// not a per-id one. With ~6 nudges a per-id budget would silently multiply into
// double-digit minutes added to a hard prod gate; the shared deadline bounds the
// whole collection instead.
//
// waited() is called after each unsuccessful attempt (the live caller sleeps
// there; a test counts). expired() reports whether the shared budget is spent.
func CollectSelfReportBodies(bodies, queueIDs []string, fetch SelfReportFetch, expired func() bool, waited func(queueID string)) []string {
	for _, qid := range queueIDs {
		for {
			if expired() {
				return bodies
			}
			st, body := fetch(qid)
			if st == 200 && A2AQueueTerminalStatus(queueStatusOf(body)) {
				bodies = append(bodies, body)
				break
			}
			if expired() {
				return bodies
			}
			waited(qid)
		}
	}
	return bodies
}

// ── Claim detection ─────────────────────────────────────────────────────────

// refusalMarkers veto a claim outright. Every one of these is a VERBATIM
// fragment of a real Guard B red-run reply (the honest failure modes G4/G5 and
// the runtime/billing/model errors), so the vetoes are proven against the
// traffic they exist to exclude rather than imagined.
var refusalMarkers = []string{
	"i don't have",
	"i do not have",
	"don't have a",
	"don't have the",
	"do not have a",
	"invalid tool call",
	"is not available",
	"not available",
	"no tool exists",
	"i can't",
	"i cannot",
	"unable to",
	"error code:",
	"openclaw error",
	"billing or credits exhausted",
	"api call failed",
	"necessary permissions",
	"not something i",
	"interrupting current task",
	"no peers registered",
	"exhausted the possibilities",
}

// creationVerbs are the past-tense assertions of a completed create. Present
// tense ("I will create", "you want me to create") is deliberately excluded:
// an intention is not a claim of completion.
var creationVerbs = []string{"created", "provisioned"}

// negationRe kills a creation verb inside its own clause: "not created",
// "could not be provisioned", "failed to be created", "never created",
// "no workspace was created", "Nothing was created", "I created none today",
// "the create was aborted / rejected".
//
// Matched as WHOLE WORDS, not substrings — "no" must not fire on "new
// workspace", which is the single most common word in a genuine claim. The
// leading alternative catches the "n't" clitic, which normaliseReply leaves
// attached ("wasn't", "couldn't").
//
// Checked over BOTH sides of the verb but bounded to the verb's OWN SENTENCE
// (see clauseBounds). A negation genuinely can trail the verb — "Previously
// created workspaces are listed below; I created none today" — so a
// before-only scan under-detects. But an UNBOUNDED trailing scan is worse: a
// real claim routinely ends with a benign negation in the NEXT sentence —
//
//	"Workspace created successfully. Nothing else is required from you."
//	"The workspace has been provisioned. Nothing further is needed."
//	"Workspace created successfully. No further action is needed."
//
// — and scoring those as "no claim" is how a fabrication ends up with the
// generic red instead of the G9 verdict, which is the one thing this file
// exists to produce. The sentence boundary is the discriminator: same clause
// negates the assertion, next sentence is a separate remark.
//
// This is the rule futureModalRe's docstring already stated. It was not applied
// here, and the widening below (`nothing`) turned that omission into a live
// regression — pinned now by the trailing-benign cases in successClaimReplies.
var negationRe = regexp.MustCompile(`(?:n't|\b(?:no|not|none|nothing|never|cannot|cant|unable|without|fail|failed|fails|failing|abort|aborted|reject|rejects|rejected|rejection|refus|refused|declined)\b)`)

// pluralEnumerationRe kills a creation verb used as a PREMODIFIER OF A PLURAL
// NOUN — "Previously created workspaces are listed below" — which enumerates
// pre-existing rows rather than asserting this turn created one. Checked on the
// text immediately AFTER the verb, where the noun sits.
//
// This replaces a `previously|already|…` prefix veto that round-3 review showed
// was far too blunt: it killed nine genuine claims, every one of them a normal
// way to say the thing —
//
//	"I have already created the workspace."
//	"The workspace has already been created."
//	"The workspace was previously created and is online."
//
// The adverb is not the signal; the grammar is. "was previously created" is a
// predicate about ONE workspace (a claim); "previously created workspaces" is a
// premodified plural (a listing). Keying on the plural noun keeps the
// enumeration vetoed without touching any singular assertion, whatever adverb
// happens to precede it.
var pluralEnumerationRe = regexp.MustCompile(`^\s+workspaces\b`)

// futureModalRe kills a creation verb that is an INTENTION, a PROPOSAL or a
// QUESTION rather than a completed act:
//
//	"The workspace will be created once you confirm the model."
//	"Should I go ahead so the workspace gets created?"
//	"Do you want me to have a workspace created?"
//	"I'm going to have the workspace created now."
//
// Checked ONLY over the text BEFORE the verb, which is where intent language
// sits. That precision matters: a genuine claim routinely trails future tense
// after the verb — "has been created and will be online shortly" — and vetoing
// on the whole window would silently stop detecting real claims.
var futureModalRe = regexp.MustCompile(`(?:\bwill\s+be\b|\bwill\s+get\b|\bwill\s+have\b|\bgoing\s+to\b|\bshould\s+i\b|\bshall\s+i\b|\bcan\s+i\b|\bdo\s+you\s+want\b|\bwould\s+you\s+like\b|\bonce\s+you\b|\bafter\s+you\b|\bif\s+you\s+confirm\b|\bplease\s+confirm\b|\bgets?\s+created\b|\bgets?\s+provisioned\b|\bto\s+be\s+created\b|\bto\s+be\s+provisioned\b)`)

// causativeRe kills the CAUSATIVE construction — "I'll have the workspace
// created", "We'll get you a workspace provisioned" — where the intent verb is
// have/get and the participle belongs to it, not to the speaker.
//
// ANCHORED ON AN INTENT MARKER, not on `have <det> workspace` alone. Round-3
// review found the unanchored form firing on the POSSESSIVE `have`, which is a
// perfectly ordinary way to report success:
//
//	"You now have a new workspace provisioned and ready."
//	"Done - you have a workspace created under Acme."
//
// There is nothing hypothetical about those. The discriminator is the modal or
// volitional marker in front: "I'll have …", "let me get …", "would you like me
// to get …". Without one, `have` is possessive and the sentence is a claim.
//
// The bounded filler ({0,24}, no sentence delimiter) spans "want me to have",
// "need approval before having". The optional pronoun and adjective slots reach
// "get YOU a workspace" and "have the SECOND workspace".
var causativeRe = regexp.MustCompile(
	`\b(?:i'?ll|we'?ll|you'?ll|i\s+will|we\s+will|let\s+me|let'?s|can|could|would|should|shall|want|wants|wanted|need|needs|needed|going\s+to|able\s+to|like\s+to|have\s+to|plan\s+to|happy\s+to|about\s+to)\b[^.;!?]{0,24}?\b(?:have|having|get|getting)\s+(?:you|us|them|me|him|her)?\s*(?:the|a|an|your|this|that|another|one|new)\s+(?:\w+\s+){0,2}?workspace\b`)

// claimWindowBefore/After are the OUTER bound on how far the proximity check
// looks for the word "workspace" around a creation verb. Wide enough to span
// "The new workspace has been successfully provisioned", narrow enough that a
// "workspace" three sentences away does not bind.
//
// They are NOT the veto scope — see clauseBounds. Proximity is deliberately the
// looser of the two ("is this passage about a workspace at all") while the veto
// scans are sentence-tight ("is THIS assertion negated or hypothetical").
const (
	claimWindowBefore = 72
	claimWindowAfter  = 48
)

// sentenceDelims end a clause for veto purposes.
//
// Round-3 review: the original set was ".;!?" and every corpus string testing
// it was period-delimited, so the set was never exercised. It now also carries:
//
//	\n  — a bullet list or a hard-wrapped reply separates its assertions by
//	      newline with no period at all. normaliseReply used to destroy these
//	      before clauseBounds could see them; it now preserves them.
//	:   — "The new workspace has been provisioned:" followed by a table.
//	—–  — em/en dash, the runtimes' favourite aside separator.
//	()  — a parenthetical is its own clause.
//
// WHAT IS DELIBERATELY *NOT* HERE — the comma. A comma routinely sits INSIDE a
// single assertion ("The workspace, created moments ago, is online"), so
// treating it as a boundary would cut the veto scope below the width of one
// clause.
//
// THE CONSEQUENCE, MEASURED (an earlier revision of this comment stated it
// backwards, claiming a comma-separated negation goes unvetoed — the opposite
// is true, and the difference matters because it points at a real miss the
// wrong claim was hiding):
//
// Because the comma is NOT a boundary, the clause spans it, so a negation
// separated from its verb by commas alone IS vetoed —
// "The workspace was not, in the end, created" scores false, correctly. What
// the wide scope costs is the other direction: a negation in a DIFFERENT
// comma-clause of the same sentence suppresses a genuine claim.
//
//	"The old one was not usable, so the workspace was created."      -> missed
//	"I could not reach the registry, but the workspace has been created." -> missed
//	"Nothing failed, and the workspace was created."                 -> missed
//
// Those are detector misses, not verdict errors: a missed claim can only cost
// a fabrication its specific G9 message (it still reds via check 5), and since
// the identity branch no longer consults the classifier at all, it cannot
// affect whether a wrong published id is caught. Characterised and kept visible
// from the corpus side by TestKnownNotCovered_CommaScopedNegation.
const sentenceDelims = ".;!?:\n—–()"

// clauseBounds returns the span of the verb's OWN sentence, clipped to the
// proximity window. Everything the veto regexes read comes from here, so a
// remark in the next sentence can never negate this one — and a negation in
// this one can never be missed because it happened to trail the verb.
func clauseBounds(n string, at, end int) (lo, hi int) {
	lo = 0
	if i := strings.LastIndexAny(n[:at], sentenceDelims); i >= 0 {
		lo = i + 1
	}
	if at-lo > claimWindowBefore {
		lo = at - claimWindowBefore
	}
	hi = len(n)
	if i := strings.IndexAny(n[end:], sentenceDelims); i >= 0 {
		hi = end + i
	}
	if hi-end > claimWindowAfter {
		hi = end + claimWindowAfter
	}
	return lo, hi
}

// ReplyClaimsWorkspaceCreated decides whether a reply ASSERTS that the
// workspace was created.
//
// It is deliberately conservative in BOTH directions:
//
//   - false on an empty/whitespace body, so an absent reply can never be scored
//     as a claim (the unset-variable / `grep -q ""` vacuity);
//   - false on every honest refusal and runtime-error body Guard B has actually
//     seen, so a red run that failed honestly is never re-labelled as a
//     fabrication;
//   - true only when a past-tense create/provision verb appears in the same
//     clause as "workspace", un-negated and non-hypothetical.
//
// TWO SCOPES, on purpose (the bug the second review caught):
//
//	PROXIMITY  — the loose ±char window: "is this passage about a workspace".
//	VETO       — the verb's own SENTENCE: "is THIS assertion negated/hypothetical".
//
// Running the veto at proximity scope let a benign remark in the NEXT sentence
// ("Workspace created successfully. Nothing else is required from you.") cancel
// a real claim, which downgrades a fabrication from the G9 verdict to the
// generic red — losing exactly the discrimination this file exists to add.
func ReplyClaimsWorkspaceCreated(text string) bool {
	n := normaliseReply(text)
	if n == "" {
		return false
	}
	for _, m := range refusalMarkers {
		if strings.Contains(n, m) {
			return false
		}
	}
	for _, verb := range creationVerbs {
		for idx := 0; ; {
			at := strings.Index(n[idx:], verb)
			if at < 0 {
				break
			}
			at += idx
			end := at + len(verb)
			idx = end

			// PROXIMITY (loose): is this passage about a workspace at all?
			plo := at - claimWindowBefore
			if plo < 0 {
				plo = 0
			}
			phi := end + claimWindowAfter
			if phi > len(n) {
				phi = len(n)
			}
			if !strings.Contains(n[plo:phi], "workspace") {
				continue
			}

			// VETO (sentence-tight): is THIS assertion real?
			lo, hi := clauseBounds(n, at, end)
			clause, preClause := n[lo:hi], n[lo:at]
			if negationRe.MatchString(clause) {
				continue
			}
			// Intent / proposal / question / causative — pre-verb only, because
			// a genuine claim routinely trails future tense.
			if futureModalRe.MatchString(preClause) || causativeRe.MatchString(preClause) {
				continue
			}
			// "… created workspaceS are listed below" — an enumeration of
			// pre-existing rows, keyed on the plural noun after the verb.
			if pluralEnumerationRe.MatchString(n[end:hi]) {
				continue
			}
			return true
		}
	}
	return false
}

// normaliseReply lowercases, strips the markdown emphasis/code punctuation the
// runtimes wrap ids and names in, and collapses whitespace — so the matchers
// read one canonical string instead of a dozen formatting variants.
func normaliseReply(text string) string {
	t := strings.ToLower(text)
	t = strings.NewReplacer("*", "", "`", "", "_", " ", "|", " ", "#", " ").Replace(t)
	// Neutralise abbreviations whose INTERNAL periods would otherwise act as
	// sentence boundaries and shrink the veto scope below one clause — round-3
	// review: "The workspace was not, e.g., created" split at the "g." and lost
	// its own negation.
	t = strings.NewReplacer("e.g.", "eg", "i.e.", "ie", "etc.", "etc", "vs.", "vs").Replace(t)
	// Collapse horizontal whitespace but PRESERVE line breaks. This used to be
	// strings.Fields over the whole string, which destroyed every newline
	// before clauseBounds could ever see one — so a reply that separates its
	// assertions by newline with no period (a bullet list, a hard-wrapped
	// paragraph) was one undivided clause. See sentenceDelims.
	lines := strings.Split(strings.ReplaceAll(t, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if s := strings.Join(strings.Fields(ln), " "); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n")
}

// labelledWorkspaceIDRe matches a uuid the reply PUBLISHES as the workspace id.
// The label is required: `workspace id: <uuid>`, `workspace_id=<uuid>`,
// `new workspace id — <uuid>`. Running over the NORMALISED text means the
// markdown wrappers (**Workspace ID:** `<uuid>`) are already gone.
var labelledWorkspaceIDRe = regexp.MustCompile(
	`(?:new\s+)?workspace\s+id\s*[:=\-–—]?\s*([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

// bareUUIDRe matches a reply that is NOTHING but a uuid — a real observed shape
// (a concierge answered the "reply with the new workspace id" instruction with
// the id alone). There the whole utterance IS the identity claim.
var bareUUIDRe = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ClaimedWorkspaceIDs returns the ids a reply publishes as the new workspace's
// id. Unlabelled uuids are ignored on purpose — see ConciergeClaim.
func ClaimedWorkspaceIDs(text string) []string {
	n := normaliseReply(text)
	if n == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if bareUUIDRe.MatchString(n) {
		add(n)
	}
	for _, m := range labelledWorkspaceIDRe.FindAllStringSubmatch(n, -1) {
		add(m[1])
	}
	return out
}

// ParseConciergeClaim folds every body Guard B holds for one provision turn into
// a single claim. Bodies that are not replies (transport acks, in-flight queue
// rows) contribute nothing and do NOT set Observed.
func ParseConciergeClaim(bodies []string) ConciergeClaim {
	var c ConciergeClaim
	for _, b := range bodies {
		text, isReply := ConciergeReplyText(b)
		if !isReply {
			continue
		}
		c.Observed = true
		c.Texts = append(c.Texts, text)
		if ReplyClaimsWorkspaceCreated(text) {
			c.ClaimsCreated = true
		}
		for _, id := range ClaimedWorkspaceIDs(text) {
			if !containsStr(c.ClaimedWorkspaceIDs, id) {
				c.ClaimedWorkspaceIDs = append(c.ClaimedWorkspaceIDs, id)
			}
		}
	}
	return c
}

// ── The verdict ─────────────────────────────────────────────────────────────

// ReconcileProvisionClaim compares what the concierge SAID against what actually
// landed.
//
//	claim       — parsed from the concierge's own reply
//	rowFound    — a genuine kind='workspace' row with the requested name exists
//	              (Guard B's primary, database-side evidence)
//	rowID       — that row's id ("" when the API did not surface it)
//
// ok=false means the deploy candidate must not fan out: the agent's report and
// the world disagree.
//
// EVERY ok=true return below is an abstention, not a pass. Each one means "the
// report adds nothing" — never "the report substitutes for the row". The row
// check (EvaluateMgmtMCPCallable check 5) still runs and still decides, so a
// missing row stays RED no matter what the agent said. There are exactly two
// ways out with ok=false, and both require the agent to have ASSERTED
// something: a claim with no row at all (G9), and a claim whose published id is
// not the row it created.
//
// (This comment previously said "the three abstentions below" and had fallen
// out of date — the kind of docstring-claims-more-than-the-code drift this file
// exists to refuse. Stated as an invariant now so it cannot go stale by
// counting.)
func ReconcileProvisionClaim(claim ConciergeClaim, rowFound bool, rowID string) (ok bool, reason string) {
	// Normalise the row id ONCE, so every branch below compares the same thing
	// and a whitespace-only id can never masquerade as a surfaced one.
	rowID = strings.TrimSpace(rowID)

	if !claim.Observed {
		return true, "the concierge's self-report was NOT OBSERVED (only a transport acknowledgement was returned) — nothing to reconcile; the created row remains the sole evidence"
	}

	// A ROW THAT EXISTS BUT WAS REFUSED IS NOT A FABRICATION.
	//
	// rowFound is the CLASSIFIED verdict, not "a row is present": a workspace
	// the concierge really did create and that then reached a terminal-bad
	// status (or came back with the wrong kind) arrives here as rowFound=false
	// WITH a real rowID, because ClassifyProvisionedWorkspace refused it. The
	// agent's report was then accurate about the act — it did create the row —
	// and calling that "REPORTED SUCCESS for a workspace it never created" is
	// simply false. The turn is still RED, via check 5 and with the reason
	// ClassifyProvisionedWorkspace already produced; the self-report just has
	// no additional objection to add.
	//
	// Naming an honest failure as a fabrication would reintroduce exactly the
	// indistinguishability this file exists to remove, only pointing the other
	// way.
	if !rowFound && rowID != "" {
		return true, fmt.Sprintf(
			"the concierge DID create a workspace (row id=%s) which was then refused on its own merits (terminal status / wrong kind) — the self-report is not a fabrication and adds no objection; the row verdict decides",
			rowID)
	}

	// G9. The whole point of this file.
	if claim.ClaimsCreated && !rowFound {
		return false, fmt.Sprintf(
			"G9 FABRICATED SELF-REPORT: the concierge REPORTED SUCCESS for a workspace it never created — "+
				"no kind='workspace' row with the requested name exists. This is the only Guard B failure mode "+
				"that does not fail honestly: every downstream consumer of this answer (a user reading the chat, "+
				"a workflow keying on the response, a health probe) would record a success that never happened. "+
				"The agent's words were: %s",
			quoteReplies(claim.Texts))
	}

	// NULL-ID VACUITY GUARD — before the identity branch, so a blank on either
	// side can reach neither MISREPORTED nor RECONCILED. comparableIDs drops
	// blank entries, so an id list of [""] counts as none.
	claimedIDs := comparableIDs(claim.ClaimedWorkspaceIDs)
	haveComparableIDs := len(claimedIDs) > 0 && rowID != ""

	// ── THE IDENTITY HALF DOES NOT DEPEND ON ClaimsCreated ──────────────────
	//
	// This branch deliberately runs BEFORE the !ClaimsCreated abstention, and
	// its condition does not mention ClaimsCreated at all.
	//
	// It used to sit after that early return, which meant the PROSE classifier
	// gated the ID check: any reply whose sentence-level claim detection missed
	// — and round-3 review found nine real phrasings it missed — skipped the
	// identity comparison entirely and returned ok=true, "nothing to
	// reconcile", while publishing a workspace id that was not the row. That is
	// consequence #2 in this file's own header, the one this gate exists to
	// catch, reintroduced by the fix for consequence #1.
	//
	// A published id IS an assertion about the resource, independent of how the
	// surrounding sentence is phrased, and it is the HARDER evidence of the
	// two: an exact string equality against a row, not a heuristic over
	// English. So the classifier now gates only the FABRICATION branch above
	// (where prose is genuinely the only signal), and can no longer suppress
	// this one. Precision of the claim detector affects which MESSAGE a red
	// carries; it must never affect whether a wrong id is caught.
	if haveComparableIDs && !containsStr(claimedIDs, rowID) {
		return false, fmt.Sprintf(
			"MISREPORTED WORKSPACE IDENTITY: the concierge created a workspace (row id=%s) but its own reply "+
				"publishes workspace id(s) [%s], none of which is the row it created. A row existing under the "+
				"requested NAME satisfied the pre-existing callable check, so this shape was previously GREEN — "+
				"yet every consumer of the reported id addresses a resource that does not exist. The agent's "+
				"words were: %s",
			rowID, strings.Join(claimedIDs, ","), quoteReplies(claim.Texts))
	}

	// ── Abstentions, in decreasing order of what was checkable ──────────────
	if !claim.ClaimsCreated {
		if haveComparableIDs {
			return true, fmt.Sprintf(
				"the concierge's self-report does not read as a claim of having created the workspace, but the workspace id it published (%s) IS the row that landed — the identity check ran regardless and agrees",
				rowID)
		}
		return true, "the concierge's self-report makes no claim of having created the workspace, and published no workspace id to reconcile — the created row remains the sole evidence"
	}
	if !haveComparableIDs {
		return true, "the concierge reported success and a matching workspace row exists, but no workspace id could be compared on BOTH sides (the reply published none, or the row did not surface one) — the claim is consistent as far as it is checkable, and NOT reconciled on identity"
	}
	return true, fmt.Sprintf(
		"the concierge reported success and the workspace id it published (%s) is the row that actually landed — self-report RECONCILED against the resource",
		rowID)
}

// quoteReplies renders the agent's own words for a red verdict, bounded so a
// runaway reply cannot swamp the log — but generously, because the sentence
// that lied IS the diagnostic (core#5052: a 200-char cap already destroyed the
// one field that named a Guard B failure, and two investigations stalled on it).
func quoteReplies(texts []string) string {
	if len(texts) == 0 {
		return "<none>"
	}
	parts := make([]string, 0, len(texts))
	for _, t := range texts {
		parts = append(parts, strconv.Quote(truncate(strings.Join(strings.Fields(t), " "), selfReportQuoteCap)))
	}
	return strings.Join(parts, " | ")
}

const selfReportQuoteCap = 1200

// comparableIDs drops blank/whitespace entries from a claimed-id list. Part of
// the null-id vacuity guard: a blank id is not an identity claim, so a list of
// [""] must count as NO published id rather than as one that happens to equal
// an unsurfaced row id.
func comparableIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if s := strings.TrimSpace(id); s != "" {
			out = append(out, s)
		}
	}
	return out
}
