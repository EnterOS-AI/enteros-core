package models

// PRODUCER-side SSOT gate for the workspace<->platform comms contract.
//
// workspace.go's RegisterPayload / HeartbeatPayload / RuntimeMetadata are the
// WIRE AUTHORITY the molecule-ai-sdk workspace-comms contract was derived from.
// This gate reflects over those real core structs and compares them with the
// generated SDK binding in go.moleculesai.app/sdk/gen/go/molcontracts. It keeps
// the check hermetic/offline while avoiding local JSON schema mirrors in core.
//
// Scope: this gate pins JSON field set and requiredness. It deliberately does
// not compare Go field types, matching the previous JSON-schema fixture gate.

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	molcontracts "go.moleculesai.app/sdk/gen/go/molcontracts"
)

// structField is the gate-relevant view of one struct field.
type structField struct {
	jsonName string
	required bool
}

// structJSONFields reflects over a struct type and returns its json-tagged
// fields keyed by json name. The required predicate lets core use
// binding:"required" while SDK generated types use the absence of ",omitempty".
func structJSONFields(t *testing.T, typ reflect.Type, required func(reflect.StructField) bool) map[string]structField {
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
		parts := strings.Split(tag, ",")
		jsonName := parts[0]
		if jsonName == "" || jsonName == "-" {
			continue
		}
		out[jsonName] = structField{jsonName: jsonName, required: required(f)}
	}
	return out
}

func coreRequired(f reflect.StructField) bool {
	for _, part := range strings.Split(f.Tag.Get("binding"), ",") {
		if strings.TrimSpace(part) == "required" {
			return true
		}
	}
	return false
}

func sdkRequired(f reflect.StructField) bool {
	tag := f.Tag.Get("json")
	for _, part := range strings.Split(tag, ",")[1:] {
		if strings.TrimSpace(part) == "omitempty" {
			return false
		}
	}
	return tag != "" && tag != "-"
}

// checkStructAgainstSDK is the gate predicate. It returns a sorted list of
// human-readable mismatch messages between a core producer struct and the
// generated SDK request/definition struct. An empty result means the two agree
// on field set and requiredness.
func checkStructAgainstSDK(t *testing.T, label string, coreType, sdkType reflect.Type) []string {
	t.Helper()
	coreFields := structJSONFields(t, coreType, coreRequired)
	sdkFields := structJSONFields(t, sdkType, sdkRequired)

	var problems []string

	for name := range sdkFields {
		if _, ok := coreFields[name]; !ok {
			problems = append(problems, label+": SDK field "+quote(name)+" has NO matching core json field (producer drifted: field removed/renamed in workspace.go?)")
		}
	}

	for name := range coreFields {
		if _, ok := sdkFields[name]; !ok {
			problems = append(problems, label+": core json field "+quote(name)+" is ABSENT from the SDK contract (producer drifted: field added to workspace.go without updating molecule-ai-sdk)")
		}
	}

	for name, sdk := range sdkFields {
		core, ok := coreFields[name]
		if !ok {
			continue
		}
		if sdk.required && !core.required {
			problems = append(problems, label+": SDK requires "+quote(name)+` but the core field lacks binding:"required" (requiredness drift)`)
		}
	}

	for name, core := range coreFields {
		sdk, ok := sdkFields[name]
		if ok && core.required && !sdk.required {
			problems = append(problems, label+": core field "+quote(name)+` is binding:"required" but the SDK contract does NOT mark it required (requiredness drift)`)
		}
	}

	sort.Strings(problems)
	return problems
}

func quote(s string) string { return `"` + s + `"` }

// TestRegisterPayload_MatchesSSOT gates models.RegisterPayload against the
// generated SDK register request contract.
func TestRegisterPayload_MatchesSSOT(t *testing.T) {
	if got := checkStructAgainstSDK(t, "RegisterPayload", reflect.TypeOf(RegisterPayload{}), reflect.TypeOf(molcontracts.RegisterRequest{})); len(got) != 0 {
		t.Fatalf("RegisterPayload drifted from the workspace-comms SDK register request:\n  - %s", strings.Join(got, "\n  - "))
	}
}

// TestHeartbeatPayload_MatchesSSOT gates models.HeartbeatPayload against the
// generated SDK heartbeat request contract.
func TestHeartbeatPayload_MatchesSSOT(t *testing.T) {
	if got := checkStructAgainstSDK(t, "HeartbeatPayload", reflect.TypeOf(HeartbeatPayload{}), reflect.TypeOf(molcontracts.HeartbeatRequest{})); len(got) != 0 {
		t.Fatalf("HeartbeatPayload drifted from the workspace-comms SDK heartbeat request:\n  - %s", strings.Join(got, "\n  - "))
	}
}

// TestRuntimeMetadata_MatchesSSOT gates models.RuntimeMetadata against the
// generated SDK heartbeat runtimeMetadata definition.
func TestRuntimeMetadata_MatchesSSOT(t *testing.T) {
	if got := checkStructAgainstSDK(t, "RuntimeMetadata", reflect.TypeOf(RuntimeMetadata{}), reflect.TypeOf(molcontracts.HeartbeatRuntimeMetadata{})); len(got) != 0 {
		t.Fatalf("RuntimeMetadata drifted from the workspace-comms SDK runtimeMetadata definition:\n  - %s", strings.Join(got, "\n  - "))
	}
}

// TestAgentCard_IsRawMessageWithRequiredName documents the agent_card seam: on
// the core wire structs it is json.RawMessage (untyped). We still pin the
// load-bearing SDK fact: the shared card shape requires a non-empty `name`.
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

	for _, tc := range []struct {
		name string
		typ  reflect.Type
	}{
		{"AgentCard", reflect.TypeOf(molcontracts.AgentCard{})},
		{"RegisterAgentCard", reflect.TypeOf(molcontracts.RegisterAgentCard{})},
		{"HeartbeatAgentCard", reflect.TypeOf(molcontracts.HeartbeatAgentCard{})},
	} {
		f, ok := tc.typ.FieldByName("Name")
		if !ok {
			t.Errorf("%s has no Name field", tc.name)
			continue
		}
		if got := strings.Split(f.Tag.Get("json"), ",")[0]; got != "name" {
			t.Errorf("%s.Name json tag is %q, expected name", tc.name, got)
		}
		if !sdkRequired(f) {
			t.Errorf("%s.Name must be required in the SDK contract", tc.name)
		}
	}
}

// TestCommsSSOTGate_CatchesDrift proves the gate reds on the main drift
// classes it exists to catch.
func TestCommsSSOTGate_CatchesDrift(t *testing.T) {
	sdkRegister := reflect.TypeOf(molcontracts.RegisterRequest{})

	// Drift A: a required field's binding:"required" was dropped.
	type registerReqRelaxed struct {
		ID           string          `json:"id"` // binding:"required" removed
		URL          string          `json:"url"`
		AgentCard    json.RawMessage `json:"agent_card" binding:"required"`
		DeliveryMode string          `json:"delivery_mode,omitempty"`
		Kind         string          `json:"kind,omitempty"`
		MCPPresent   *bool           `json:"mcp_server_present,omitempty"`
	}
	if got := checkStructAgainstSDK(t, "registerReqRelaxed", reflect.TypeOf(registerReqRelaxed{}), sdkRegister); len(got) == 0 {
		t.Fatal(`gate blind: a dropped binding:"required" on id was not caught`)
	}

	// Drift B: a producer-only field was added without updating the SDK SSOT.
	type registerReqExtra struct {
		ID           string          `json:"id" binding:"required"`
		URL          string          `json:"url"`
		AgentCard    json.RawMessage `json:"agent_card" binding:"required"`
		DeliveryMode string          `json:"delivery_mode,omitempty"`
		Kind         string          `json:"kind,omitempty"`
		MCPPresent   *bool           `json:"mcp_server_present,omitempty"`
		Extra        string          `json:"extra_field,omitempty"`
	}
	if got := checkStructAgainstSDK(t, "registerReqExtra", reflect.TypeOf(registerReqExtra{}), sdkRegister); len(got) == 0 {
		t.Fatal("gate blind: an added producer-only field was not caught")
	}

	// Drift C: a contract field was removed/renamed in the producer.
	type registerReqMissing struct {
		ID           string `json:"id" binding:"required"`
		URL          string `json:"url"`
		DeliveryMode string `json:"delivery_mode,omitempty"`
		Kind         string `json:"kind,omitempty"`
		MCPPresent   *bool  `json:"mcp_server_present,omitempty"`
	}
	if got := checkStructAgainstSDK(t, "registerReqMissing", reflect.TypeOf(registerReqMissing{}), sdkRegister); len(got) == 0 {
		t.Fatal("gate blind: a missing contract field was not caught")
	}
}
