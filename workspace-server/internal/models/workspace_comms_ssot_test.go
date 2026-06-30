package models

// PRODUCER-side SSOT gate for the workspace<->platform comms contract.
//
// Why this exists — the gap this closes
// -------------------------------------
// workspace.go's RegisterPayload / HeartbeatPayload / RuntimeMetadata are the
// WIRE AUTHORITY the molecule-contracts `workspace-comms/*.schema.json` SSOT
// schemas were DERIVED FROM. But nothing pinned the two together: the only
// pre-existing comms contract test (registry_payload_contract_test.go) pins
// core-struct <-> runtime-Python (the CONSUMER side / the bytes the runtime
// emits), NOT core-struct <-> the SSOT schema. molecule-contracts ships
// gen/go/workspace_comms_gen.go but core never imports it. So an edit to
// workspace.go (rename a json tag, add/drop a field, flip a binding:"required")
// could silently drift the SSOT and no gate would red.
//
// This gate closes that: it reflects over the real structs and asserts they are
// field-compatible with the VENDORED workspace-comms schemas — every schema
// `required` field has a struct field with the right json tag AND a
// binding:"required" tag; the struct's json-tagged fields and the schema's
// request `properties` are the SAME SET (extras OR removals on either side red).
//
// Vendored, not fetched (testdata/workspace-comms/*.schema.json): the gate is
// hermetic/offline so `go test ./...` reds on a real divergence, never a network
// blip. The vendored copies are kept honest against molecule-contracts by the
// `contract-ssot-sync` workflow (see that dir's README.md). When the contract
// changes, re-sync testdata/ from molecule-contracts and update workspace.go.

import (
	"embed"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

//go:embed testdata/workspace-comms/register.schema.json
//go:embed testdata/workspace-comms/heartbeat.schema.json
//go:embed testdata/workspace-comms/agent-card.schema.json
var commsSchemaFS embed.FS

// schemaNode is the minimal recursive shape of a JSON-Schema (draft 2020-12)
// node we need to walk: required[], properties{}, $defs{}, $ref. Everything
// else (descriptions, types, enums) is irrelevant to the field-set/requiredness
// gate and is ignored.
type schemaNode struct {
	Type       string                `json:"type"`
	Required   []string              `json:"required"`
	Properties map[string]schemaNode `json:"properties"`
	Defs       map[string]schemaNode `json:"$defs"`
	Ref        string                `json:"$ref"`
}

func loadSchema(t *testing.T, name string) schemaNode {
	t.Helper()
	b, err := commsSchemaFS.ReadFile("testdata/workspace-comms/" + name)
	if err != nil {
		t.Fatalf("read vendored schema %s: %v (re-sync testdata/ from molecule-contracts)", name, err)
	}
	var n schemaNode
	if err := json.Unmarshal(b, &n); err != nil {
		t.Fatalf("parse vendored schema %s: %v", name, err)
	}
	return n
}

// structField is the gate-relevant view of one struct field.
type structField struct {
	jsonName       string
	bindingRequire bool
}

// structJSONFields reflects over a struct type and returns its json-tagged
// fields keyed by json name, recording whether each carries binding:"required".
// Fields tagged json:"-" or with no json tag are skipped (not on the wire).
func structJSONFields(t *testing.T, typ reflect.Type) map[string]structField {
	t.Helper()
	if typ.Kind() != reflect.Struct {
		t.Fatalf("structJSONFields: %s is not a struct", typ)
	}
	out := map[string]structField{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		jsonName := strings.Split(tag, ",")[0]
		if jsonName == "" || jsonName == "-" {
			continue
		}
		binding := f.Tag.Get("binding")
		req := false
		for _, part := range strings.Split(binding, ",") {
			if strings.TrimSpace(part) == "required" {
				req = true
			}
		}
		out[jsonName] = structField{jsonName: jsonName, bindingRequire: req}
	}
	return out
}

// checkStructAgainstSchema is the gate predicate. It returns a sorted list of
// human-readable mismatch messages between a Go struct and a JSON-Schema object
// node (the `request` sub-schema, or a $defs sub-schema). An EMPTY result means
// the struct and the schema agree on (a) field set and (b) requiredness.
//
// It is a pure function of (struct type, schema node) so the test below can run
// it against the REAL structs (expect: no mismatches) and against a deliberately
// drifted struct (expect: mismatches) — proving the gate actually catches drift.
func checkStructAgainstSchema(t *testing.T, label string, typ reflect.Type, node schemaNode) []string {
	t.Helper()
	fields := structJSONFields(t, typ)

	var problems []string

	schemaProps := map[string]bool{}
	for name := range node.Properties {
		schemaProps[name] = true
	}
	schemaRequired := map[string]bool{}
	for _, name := range node.Required {
		schemaRequired[name] = true
	}

	// (1) Every schema property must have a struct field (no SSOT-only field
	// that the producer fails to carry).
	for name := range schemaProps {
		if _, ok := fields[name]; !ok {
			problems = append(problems, label+": schema property "+quote(name)+" has NO matching struct json field (producer drifted: field removed/renamed in workspace.go?)")
		}
	}

	// (2) Every struct json field must be a schema property (no producer-only
	// field that silently drifts the SSOT — the core defect this gate closes).
	for name := range fields {
		if !schemaProps[name] {
			problems = append(problems, label+": struct json field "+quote(name)+" is ABSENT from the SSOT schema properties (producer drifted: field added to workspace.go without updating the molecule-contracts schema)")
		}
	}

	// (3) Every schema `required` field must exist as a struct field AND carry
	// binding:"required" — the schema's requiredness mirrors the Go binding
	// tags (see the schema descriptions), so a flipped binding tag is drift.
	for name := range schemaRequired {
		f, ok := fields[name]
		if !ok {
			problems = append(problems, label+": schema requires "+quote(name)+" but no struct json field has that name")
			continue
		}
		if !f.bindingRequire {
			problems = append(problems, label+": schema requires "+quote(name)+` but the struct field lacks binding:"required" (requiredness drift)`)
		}
	}

	// (4) Belt-and-suspenders the other way: a struct field tagged
	// binding:"required" that the schema does NOT mark required is also drift.
	for name, f := range fields {
		if f.bindingRequire && schemaProps[name] && !schemaRequired[name] {
			problems = append(problems, label+": struct field "+quote(name)+` is binding:"required" but the SSOT schema does NOT list it as required (requiredness drift)`)
		}
	}

	sort.Strings(problems)
	return problems
}

func quote(s string) string { return `"` + s + `"` }

// requestNode pulls the `request` sub-schema out of a register/heartbeat
// contract schema (which models both `request` and `response`). The Go wire
// struct is the request body.
func requestNode(t *testing.T, root schemaNode) schemaNode {
	t.Helper()
	req, ok := root.Properties["request"]
	if !ok {
		t.Fatal("schema has no properties.request node")
	}
	return req
}

// TestRegisterPayload_MatchesSSOT gates models.RegisterPayload against the
// vendored register.schema.json request sub-shape.
func TestRegisterPayload_MatchesSSOT(t *testing.T) {
	schema := requestNode(t, loadSchema(t, "register.schema.json"))
	if got := checkStructAgainstSchema(t, "RegisterPayload", reflect.TypeOf(RegisterPayload{}), schema); len(got) != 0 {
		t.Fatalf("RegisterPayload drifted from the workspace-comms SSOT register schema:\n  - %s", strings.Join(got, "\n  - "))
	}
}

// TestHeartbeatPayload_MatchesSSOT gates models.HeartbeatPayload against the
// vendored heartbeat.schema.json request sub-shape.
func TestHeartbeatPayload_MatchesSSOT(t *testing.T) {
	schema := requestNode(t, loadSchema(t, "heartbeat.schema.json"))
	if got := checkStructAgainstSchema(t, "HeartbeatPayload", reflect.TypeOf(HeartbeatPayload{}), schema); len(got) != 0 {
		t.Fatalf("HeartbeatPayload drifted from the workspace-comms SSOT heartbeat schema:\n  - %s", strings.Join(got, "\n  - "))
	}
}

// TestRuntimeMetadata_MatchesSSOT gates models.RuntimeMetadata against the
// $defs/runtimeMetadata sub-schema inside heartbeat.schema.json.
func TestRuntimeMetadata_MatchesSSOT(t *testing.T) {
	root := loadSchema(t, "heartbeat.schema.json")
	defNode, ok := root.Defs["runtimeMetadata"]
	if !ok {
		t.Fatal("heartbeat schema has no $defs/runtimeMetadata")
	}
	if got := checkStructAgainstSchema(t, "RuntimeMetadata", reflect.TypeOf(RuntimeMetadata{}), defNode); len(got) != 0 {
		t.Fatalf("RuntimeMetadata drifted from the workspace-comms SSOT runtimeMetadata $def:\n  - %s", strings.Join(got, "\n  - "))
	}
}

// TestAgentCard_IsRawMessageWithRequiredName documents the agent_card seam: on
// the wire it is json.RawMessage (untyped) on all three payload structs, so
// there is no Go struct to field-compare. We still pin the load-bearing facts
// from agent-card.schema.json: (a) the field exists on the structs as a raw
// message, and (b) the SSOT requires a non-empty `name`. If molecule-contracts
// ever gives agent_card a typed Go shape in core, replace this with a real
// field gate.
func TestAgentCard_IsRawMessageWithRequiredName(t *testing.T) {
	rawType := reflect.TypeOf(json.RawMessage(nil))
	for _, tc := range []struct {
		name string
		typ  reflect.Type
	}{
		{"RegisterPayload", reflect.TypeOf(RegisterPayload{})},
		{"HeartbeatPayload", reflect.TypeOf(HeartbeatPayload{})},
		{"UpdateCardPayload", reflect.TypeOf(UpdateCardPayload{})},
	} {
		f, ok := tc.typ.FieldByName("AgentCard")
		if !ok {
			t.Errorf("%s has no AgentCard field", tc.name)
			continue
		}
		if f.Type != rawType {
			t.Errorf("%s.AgentCard is %s, expected json.RawMessage (untyped on the wire)", tc.name, f.Type)
		}
		if got := strings.Split(f.Tag.Get("json"), ",")[0]; got != "agent_card" {
			t.Errorf("%s.AgentCard json tag is %q, expected agent_card", tc.name, got)
		}
	}

	card := loadSchema(t, "agent-card.schema.json")
	reqd := map[string]bool{}
	for _, r := range card.Required {
		reqd[r] = true
	}
	if !reqd["name"] {
		t.Error("agent-card.schema.json must require `name` (the single load-bearing card field)")
	}
}

// TestCommsSSOTGate_CatchesDrift PROVES the gate actually reds on a divergence.
// It builds deliberately-wrong structs and asserts checkStructAgainstSchema
// flags each drift class. This is the negative case for the gate above: if this
// test ever passes with an empty problem list, the gate has gone blind.
func TestCommsSSOTGate_CatchesDrift(t *testing.T) {
	registerReq := requestNode(t, loadSchema(t, "register.schema.json"))

	// Drift A: a required field's binding:"required" was dropped (someone
	// "relaxed" id). The SSOT still marks id required -> requiredness drift.
	type registerReqRelaxed struct {
		ID           string          `json:"id"` // <- binding:"required" REMOVED
		URL          string          `json:"url"`
		AgentCard    json.RawMessage `json:"agent_card" binding:"required"`
		DeliveryMode string          `json:"delivery_mode,omitempty"`
		Kind         string          `json:"kind,omitempty"`
		MCPPresent   *bool           `json:"mcp_server_present,omitempty"`
	}
	if got := checkStructAgainstSchema(t, "registerReqRelaxed", reflect.TypeOf(registerReqRelaxed{}), registerReq); len(got) == 0 {
		t.Fatal("gate BLIND: a dropped binding:\"required\" on id was not caught")
	} else {
		t.Logf("drift A (dropped required) correctly caught:\n  - %s", strings.Join(got, "\n  - "))
	}

	// Drift B: a producer-only field was added to the struct without updating
	// the SSOT schema (the headline defect this gate closes).
	type registerReqExtra struct {
		ID           string          `json:"id" binding:"required"`
		URL          string          `json:"url"`
		AgentCard    json.RawMessage `json:"agent_card" binding:"required"`
		DeliveryMode string          `json:"delivery_mode,omitempty"`
		Kind         string          `json:"kind,omitempty"`
		MCPPresent   *bool           `json:"mcp_server_present,omitempty"`
		SneakyNew    string          `json:"sneaky_new_field,omitempty"` // <- not in SSOT
	}
	if got := checkStructAgainstSchema(t, "registerReqExtra", reflect.TypeOf(registerReqExtra{}), registerReq); len(got) == 0 {
		t.Fatal("gate BLIND: a producer-only field absent from the SSOT was not caught")
	} else {
		t.Logf("drift B (producer-only extra field) correctly caught:\n  - %s", strings.Join(got, "\n  - "))
	}

	// Drift C: a schema field was renamed away in the struct (id -> identifier).
	type registerReqRenamed struct {
		Identifier   string          `json:"identifier" binding:"required"` // <- was "id"
		URL          string          `json:"url"`
		AgentCard    json.RawMessage `json:"agent_card" binding:"required"`
		DeliveryMode string          `json:"delivery_mode,omitempty"`
		Kind         string          `json:"kind,omitempty"`
		MCPPresent   *bool           `json:"mcp_server_present,omitempty"`
	}
	if got := checkStructAgainstSchema(t, "registerReqRenamed", reflect.TypeOf(registerReqRenamed{}), registerReq); len(got) == 0 {
		t.Fatal("gate BLIND: a renamed required field was not caught")
	} else {
		t.Logf("drift C (renamed field) correctly caught:\n  - %s", strings.Join(got, "\n  - "))
	}

	// Sanity: a struct that faithfully mirrors the SSOT request produces NO
	// problems (the gate is not just always-failing).
	type registerReqFaithful struct {
		ID           string          `json:"id" binding:"required"`
		URL          string          `json:"url"`
		AgentCard    json.RawMessage `json:"agent_card" binding:"required"`
		DeliveryMode string          `json:"delivery_mode,omitempty"`
		Kind         string          `json:"kind,omitempty"`
		MCPPresent   *bool           `json:"mcp_server_present,omitempty"`
	}
	if got := checkStructAgainstSchema(t, "registerReqFaithful", reflect.TypeOf(registerReqFaithful{}), registerReq); len(got) != 0 {
		t.Fatalf("gate FALSE-POSITIVE: a faithful struct was flagged:\n  - %s", strings.Join(got, "\n  - "))
	}
}
