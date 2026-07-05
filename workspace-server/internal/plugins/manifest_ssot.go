package plugins

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"gopkg.in/yaml.v3"
)

// manifestSchemaJSON is the VENDORED copy of the plugin-manifest SSOT
// schema (molecule-ai-sdk contracts/plugin-manifest/plugin-manifest.schema.json,
// see contracts/PROVENANCE.md). Kept in lockstep with the SSOT by
// .gitea/scripts/check-plugin-manifest-ssot-sync.sh.
//
//go:embed contracts/plugin-manifest.schema.json
var manifestSchemaJSON []byte

// maxManifestViolations bounds the flattened violation list so a
// pathological manifest can't blow up the advisory log line.
const maxManifestViolations = 20

var (
	manifestSchemaOnce sync.Once
	manifestSchema     *jsonschema.Schema
	manifestSchemaErr  error
)

// violationPrinter renders jsonschema ErrorKind messages. Package-level so
// flattenManifestViolations doesn't allocate a printer per violation.
var violationPrinter = message.NewPrinter(language.English)

// compiledManifestSchema compiles the embedded SSOT schema exactly once.
// A compile failure can only mean the vendored copy is corrupt (programmer
// error, guarded by TestManifestSchema_Compiles) — returned as an error
// rather than panicking so the advisory install path stays non-fatal.
func compiledManifestSchema() (*jsonschema.Schema, error) {
	manifestSchemaOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(manifestSchemaJSON))
		if err != nil {
			manifestSchemaErr = fmt.Errorf("embedded plugin-manifest schema is not valid JSON: %w", err)
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource("plugin-manifest.schema.json", doc); err != nil {
			manifestSchemaErr = fmt.Errorf("embedded plugin-manifest schema failed to load: %w", err)
			return
		}
		manifestSchema, manifestSchemaErr = c.Compile("plugin-manifest.schema.json")
	})
	return manifestSchema, manifestSchemaErr
}

// ValidateManifestSSOT validates plugin.yaml bytes against the vendored
// plugin-manifest SSOT schema (JSON-Schema draft 2020-12, core#3383).
// Returns nil/empty for a conforming manifest; otherwise a bounded list of
// human-readable violations. Callers decide the enforcement posture — the
// install pipeline is ADVISORY (log-only) until the PR-4 post-soak
// fail-closed promotion.
func ValidateManifestSSOT(manifestYAML []byte) []string {
	var manifest any
	if err := yaml.Unmarshal(manifestYAML, &manifest); err != nil {
		return []string{fmt.Sprintf("plugin.yaml is not valid YAML: %v", err)}
	}

	// JSON-normalize the YAML value so the validator sees the same shapes
	// encoding/json would produce (map[string]any / []any / float64). A
	// Marshal failure (e.g. non-string map keys) means the manifest can't
	// be a JSON object at all — which is what the schema contract is over.
	jsonBytes, err := json.Marshal(manifest)
	if err != nil {
		return []string{"manifest is not a JSON-compatible object"}
	}
	var normalized any
	if err := json.Unmarshal(jsonBytes, &normalized); err != nil {
		return []string{"manifest is not a JSON-compatible object"}
	}

	sch, err := compiledManifestSchema()
	if err != nil {
		return []string{err.Error()}
	}

	if err := sch.Validate(normalized); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			var out []string
			flattenManifestViolations(ve, &out)
			return out
		}
		return []string{fmt.Sprintf("manifest failed schema validation: %v", err)}
	}
	return nil
}

// flattenManifestViolations walks a ValidationError tree and collects
// leaf-cause messages (the actionable ones — intermediate nodes are just
// "validation failed" wrappers), each prefixed with the JSON pointer of
// the offending instance location. Capped at maxManifestViolations.
func flattenManifestViolations(ve *jsonschema.ValidationError, out *[]string) {
	if len(*out) >= maxManifestViolations {
		return
	}
	if len(ve.Causes) == 0 {
		*out = append(*out, fmt.Sprintf("at %s: %s",
			"/"+strings.Join(ve.InstanceLocation, "/"),
			ve.ErrorKind.LocalizedString(violationPrinter)))
		return
	}
	for _, cause := range ve.Causes {
		flattenManifestViolations(cause, out)
		if len(*out) >= maxManifestViolations {
			return
		}
	}
}
