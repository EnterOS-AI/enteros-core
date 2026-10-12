package handlers

// M5: the declaration surface the plugin tab renders from.
//
// The done condition is "the plugin tab renders a form for a plugin the
// frontend has never seen, added after the build". That only works if the form
// is driven by the plugin's OWN declaration rather than by anything hard-coded
// in the frontend — so this reads `contributes.configuration` live from the
// installed plugin's plugin.yaml ON THE WORKSPACE, not from a registry core
// keeps.
//
// Reading it from the box matters: the box is the only place that knows which
// version of a plugin is actually installed. A registry copy would drift the
// moment a workspace pinned an older source.

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// declaredProperty is one configurable key as the plugin declares it, plus the
// rendering hints the tab needs.
type declaredProperty struct {
	Key         string `json:"key"`
	Type        string `json:"type,omitempty"`
	Default     any    `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
	Enum        []any  `json:"enum,omitempty"`
	// Sensitive keys are credential-shaped. The tab MUST render a reference
	// picker rather than a plaintext box, and the generated .example carries a
	// placeholder — never a real value.
	Sensitive bool `json:"sensitive,omitempty"`
	Required  bool `json:"required,omitempty"`
}

// pluginDeclaration is the whole `contributes.configuration` block, normalised.
type pluginDeclaration struct {
	Title       string             `json:"title,omitempty"`
	Description string             `json:"description,omitempty"`
	Properties  []declaredProperty `json:"properties"`
}

// parsePluginDeclaration pulls `contributes.configuration` out of a plugin.yaml.
//
// TOLERANT BY CONTRACT, matching the manifest schema: the property is declared
// as an open anyOf precisely so a malformed block cannot brick the plugin. A
// block we cannot read yields an empty declaration (the tab shows no form)
// rather than an error that would make the plugin look broken.
func parsePluginDeclaration(manifestYAML []byte) (pluginDeclaration, error) {
	var doc struct {
		Contributes struct {
			Configuration struct {
				Title       string `yaml:"title"`
				Description string `yaml:"description"`
				Properties  map[string]struct {
					Type        string `yaml:"type"`
					Default     any    `yaml:"default"`
					Description string `yaml:"description"`
					Enum        []any  `yaml:"enum"`
					Sensitive   bool   `yaml:"sensitive"`
					Required    bool   `yaml:"required"`
				} `yaml:"properties"`
			} `yaml:"configuration"`
		} `yaml:"contributes"`
	}
	if err := yaml.Unmarshal(manifestYAML, &doc); err != nil {
		return pluginDeclaration{Properties: []declaredProperty{}}, fmt.Errorf("parse plugin manifest: %w", err)
	}

	cfg := doc.Contributes.Configuration
	out := pluginDeclaration{
		Title:       cfg.Title,
		Description: cfg.Description,
		Properties:  make([]declaredProperty, 0, len(cfg.Properties)),
	}
	for key, p := range cfg.Properties {
		out.Properties = append(out.Properties, declaredProperty{
			Key: key, Type: p.Type, Default: p.Default, Description: p.Description,
			Enum: p.Enum, Sensitive: p.Sensitive, Required: p.Required,
		})
	}
	// Stable order — a form whose fields reshuffle between requests is unusable.
	sort.Slice(out.Properties, func(i, j int) bool { return out.Properties[i].Key < out.Properties[j].Key })
	return out, nil
}

// errUnsafeInstallName is returned before ANY backend is touched when the
// install name could address something outside plugins/<name>/. A sentinel
// rather than a bare fmt.Errorf so a test can assert the guard actually fired:
// with no reachable backend every name errors, so `err != nil` proves nothing
// about the guard (see TestDeclaration_UnsafeInstallNameIsRefusedBeforeAnyRead).
var errUnsafeInstallName = errors.New("unsafe plugin install name")

// readPluginManifestFromWorkspace fetches an installed plugin's plugin.yaml off
// the workspace.
//
// It goes through readWorkspaceConfigFile, which is the SAME three-way backend
// dispatch writePluginSettingsToWorkspace uses. Before that it had one branch —
// findContainer → docker exec — and findContainer returns "" whenever
// h.docker == nil, i.e. on the docker-less CP tenant shape and on every
// SaaS/EC2 workspace. A declared AND installed plugin 404'd on both, and the
// endpoint was dead off local Docker.
//
// The three failure modes stay distinguishable in the returned error:
// errWorkspaceFileAbsent (not installed), errWorkspaceUnreachable (cannot see
// the box), errWorkspaceReadFailed (the backend errored).
func (h *TemplatesHandler) readPluginManifestFromWorkspace(
	ctx context.Context, workspaceID, installName string,
) ([]byte, error) {
	if installName == "" || installName != path.Base(installName) || installName == "." || installName == ".." {
		return nil, fmt.Errorf("%w: %q", errUnsafeInstallName, installName)
	}
	return h.readWorkspaceConfigFile(ctx, workspaceID, path.Join("plugins", installName, "plugin.yaml"))
}

// renderSettingsExample generates the `.example` file for a plugin's settings.
//
// Non-sensitive keys carry a real, usable example — the declared default when
// there is one, else a type-shaped placeholder — because the point of an
// example is to be copyable.
//
// SENSITIVE KEYS CARRY A PLACEHOLDER AND NEVER A VALUE. This function has no
// access to real values by construction: it takes only the declaration. That is
// deliberate — an .example generator that could see live settings is one
// refactor away from committing a credential to a repo.
func renderSettingsExample(pluginName string, decl pluginDeclaration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — per-install settings\n", pluginName)
	if decl.Description != "" {
		for _, line := range strings.Split(strings.TrimSpace(decl.Description), "\n") {
			fmt.Fprintf(&b, "# %s\n", strings.TrimSpace(line))
		}
	}
	b.WriteString("#\n# Set these under the plugin entry in your workspace template:\n")
	b.WriteString("#\n#   plugins:\n#     - source: <plugin source>\n#       config:\n#         <key>: <value>\n\n")

	for _, p := range decl.Properties {
		if p.Description != "" {
			for _, line := range strings.Split(strings.TrimSpace(p.Description), "\n") {
				fmt.Fprintf(&b, "# %s\n", strings.TrimSpace(line))
			}
		}
		if len(p.Enum) > 0 {
			fmt.Fprintf(&b, "# one of: %s\n", formatEnum(p.Enum))
		}
		if p.Required {
			b.WriteString("# REQUIRED\n")
		}
		if p.Sensitive {
			// No default is echoed even if the manifest declares one — a
			// sensitive default is still a secret-shaped string.
			fmt.Fprintf(&b, "# SENSITIVE — do not commit a real value; reference a secret instead\n%s: \"<%s>\"\n\n",
				p.Key, strings.ToUpper(strings.ReplaceAll(p.Key, "-", "_")))
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n\n", p.Key, exampleValue(p))
	}
	return b.String()
}

func formatEnum(vals []any) string {
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		parts = append(parts, fmt.Sprintf("%v", v))
	}
	return strings.Join(parts, ", ")
}

// exampleValue prefers the declared default, then a type-shaped placeholder.
func exampleValue(p declaredProperty) string {
	if p.Default != nil {
		switch v := p.Default.(type) {
		case string:
			return fmt.Sprintf("%q", v)
		case []any, map[string]any:
			if len(p.Enum) > 0 {
				return fmt.Sprintf("%v", p.Enum[0])
			}
			return "[]"
		default:
			return fmt.Sprintf("%v", v)
		}
	}
	if len(p.Enum) > 0 {
		if s, ok := p.Enum[0].(string); ok {
			return fmt.Sprintf("%q", s)
		}
		return fmt.Sprintf("%v", p.Enum[0])
	}
	switch p.Type {
	case "integer", "number":
		return "0"
	case "boolean":
		return "false"
	case "array":
		return "[]"
	case "object":
		return "{}"
	default:
		return `""`
	}
}
