// Package a2aresp is the single source of truth for extracting content from an
// A2A (agent-to-agent) JSON-RPC message/send response body.
//
// Before this package the same shape-walking logic was copy-pasted across
// handlers.extractA2AText, channels.Manager.extractReplyText, and
// messagestore's joinTextParts/extractFilesFromTask — each learned the A2A
// result shapes at a different time and DRIFTED, so a shape one copy handled
// another dropped. That drift shipped two live bugs: a 2026-07-19 Task-shape
// greeting drop and a 2026-07-26 bare-Message greeting drop (the concierge's
// real greeting was discarded and the canned fallback shipped). Consolidating
// here means the shape matrix is defined and tested ONCE (see extract_test.go);
// callers keep only their transport-specific wrappers.
//
// The A2A message/send result carries content in one of four nested shapes:
//
//	result.parts[]                    (bare Message, result.kind == "message")
//	result.message.parts[]            (nested Message)
//	result.status.message.parts[]     (Task, a2a-sdk)
//	result.artifacts[].parts[]        (Task with artifacts)
//
// Each part is either v0.3 (kind:"text"|"file") or v0.2 (type:"text"|"file")
// or a v1 protobuf-flat file (top-level url/filename/mediaType). Two text
// extractors serve two contracts: Text returns a single shape by PRECEDENCE
// (reply delivery — the agent's answer, never glued to an interim status line),
// while AllText concatenates every shape with "\n" (chat-history archival — a
// reply split across parts + artifacts is never truncated). Files collects file
// parts across all shapes.
package a2aresp

import (
	"encoding/json"
	"path"
	"strings"
)

// Text extracts the agent's reply text from an A2A response body by SHAPE
// PRECEDENCE (the first shape that yields text wins — see the body). Returns ""
// when the body has no result or no text parts (callers decide how to treat
// that — e.g. a raw-JSON diagnostic fallback). Errors are NOT surfaced here; use
// ErrorMessage. For the full-history concatenation of every shape, use AllText.
func Text(body []byte) string {
	result := resultObject(body)
	if result == nil {
		return ""
	}
	// SHAPE PRECEDENCE — the FIRST shape that yields text wins; all text parts
	// WITHIN that shape are joined with "\n" so a multi-part reply is not
	// truncated. Order: parts → artifacts → status.message → message. This
	// returns the agent's ANSWER without gluing an interim status.message line
	// in front of it or duplicating a reply mirrored across shapes — the
	// reply-extraction contract the handlers/channels call sites require. It
	// preserves the pre-consolidation behavior: a Task (no top-level parts)
	// falls to artifacts then status.message; a Message reply uses parts.
	if t := joinTextParts(partsAt(result, "parts")); t != "" {
		return t
	}
	if arts, ok := result["artifacts"].([]interface{}); ok {
		for _, a := range arts {
			if art, ok := a.(map[string]interface{}); ok {
				if t := joinTextParts(partsAt(art, "parts")); t != "" {
					return t
				}
			}
		}
	}
	if st, ok := result["status"].(map[string]interface{}); ok {
		if msg, ok := st["message"].(map[string]interface{}); ok {
			if t := joinTextParts(partsAt(msg, "parts")); t != "" {
				return t
			}
		}
	}
	if msg, ok := result["message"].(map[string]interface{}); ok {
		if t := joinTextParts(partsAt(msg, "parts")); t != "" {
			return t
		}
	}
	return ""
}

// AllText concatenates text from EVERY shape ({"result":"<string>"}, then
// parts → status.message → message → artifacts) with "\n". This is the
// full-fidelity form the persisted chat-history path uses, where a reply split
// across summary-in-parts + details-in-artifacts (claude-code / hermes) must be
// captured whole — NOT the reply-extraction Text() above, which returns a single
// shape. Callers that render or deliver a live reply want Text; callers that
// archive the complete turn want AllText.
func AllText(body []byte) string {
	// {"result": "<plain string>"} — some agents answer with a bare string.
	var asString struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(body, &asString); err == nil && asString.Result != "" {
		return asString.Result
	}
	result := resultObject(body)
	if result == nil {
		return ""
	}
	var collected []string
	add := func(parts []interface{}) {
		if t := joinTextParts(parts); t != "" {
			collected = append(collected, t)
		}
	}
	// Collection order: parts → status.message → message → artifacts.
	add(partsAt(result, "parts"))
	if st, ok := result["status"].(map[string]interface{}); ok {
		if msg, ok := st["message"].(map[string]interface{}); ok {
			add(partsAt(msg, "parts"))
		}
	}
	if msg, ok := result["message"].(map[string]interface{}); ok {
		add(partsAt(msg, "parts"))
	}
	if arts, ok := result["artifacts"].([]interface{}); ok {
		for _, a := range arts {
			if art, ok := a.(map[string]interface{}); ok {
				add(partsAt(art, "parts"))
			}
		}
	}
	return strings.Join(collected, "\n")
}

// ErrorMessage returns the A2A JSON-RPC error message ("" if the body is not an
// error response). Kept separate from Text so each caller composes error-vs-text
// precedence to its own contract.
func ErrorMessage(body []byte) string {
	var resp struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Error == nil {
		return ""
	}
	return resp.Error.Message
}

// File is a neutral file-part descriptor. Callers map it to their own type
// (e.g. messagestore.ChatAttachment).
type File struct {
	Name     string
	URI      string
	MimeType string
	Size     *int64
}

// Files extracts every file part across all four result shapes (v0 kind/type
// "file" with a nested file object, and v1 protobuf-flat with top-level
// url/filename/mediaType).
func Files(body []byte) []File {
	result := resultObject(body)
	if result == nil {
		return nil
	}
	// Shape walk order: parts → artifacts → status.message → message. This
	// mirrors the pre-consolidation messagestore.extractFilesFromTask order so
	// attachment display order is preserved (artifacts precede status/message).
	var out []File
	out = appendFiles(out, partsAt(result, "parts"))
	if arts, ok := result["artifacts"].([]interface{}); ok {
		for _, a := range arts {
			if art, ok := a.(map[string]interface{}); ok {
				out = appendFiles(out, partsAt(art, "parts"))
			}
		}
	}
	if st, ok := result["status"].(map[string]interface{}); ok {
		if msg, ok := st["message"].(map[string]interface{}); ok {
			out = appendFiles(out, partsAt(msg, "parts"))
		}
	}
	if msg, ok := result["message"].(map[string]interface{}); ok {
		out = appendFiles(out, partsAt(msg, "parts"))
	}
	return out
}

// ── internals ───────────────────────────────────────────────────────────────

// resultObject unwraps the JSON-RPC envelope to the result object. A body may
// arrive already-unwrapped (a bare task/message object) — in that case it is
// returned as-is so callers that pre-unwrap still work.
func resultObject(body []byte) map[string]interface{} {
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	if result, ok := resp["result"].(map[string]interface{}); ok {
		return result
	}
	// No "result" key: treat the body itself as the result (pre-unwrapped).
	if _, hasParts := resp["parts"]; hasParts {
		return resp
	}
	if _, hasArts := resp["artifacts"]; hasArts {
		return resp
	}
	if _, hasStatus := resp["status"]; hasStatus {
		return resp
	}
	if _, hasMsg := resp["message"]; hasMsg {
		return resp
	}
	return nil
}

func partsAt(obj map[string]interface{}, key string) []interface{} {
	if parts, ok := obj[key].([]interface{}); ok {
		return parts
	}
	return nil
}

// joinTextParts concatenates the text of every text part with "\n", skipping
// non-text parts by the kind/type discriminator. A part with NO discriminator
// but a "text" field is treated as text (permissive, matches canvas).
func joinTextParts(parts []interface{}) string {
	var texts []string
	for _, p := range parts {
		part, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		// v1 protobuf Part wraps its payload in a "root" oneof: {root:{text:…}}.
		if root, ok := part["root"].(map[string]interface{}); ok {
			if t, ok := root["text"].(string); ok && t != "" {
				texts = append(texts, t)
			}
			continue
		}
		kind, hasKind := part["kind"].(string)
		typ, hasType := part["type"].(string)
		hasDiscriminator := (hasKind && kind != "") || (hasType && typ != "")
		isText := true
		if hasDiscriminator {
			isText = kind == "text" || typ == "text"
		}
		if !isText {
			continue
		}
		if t, ok := part["text"].(string); ok && t != "" {
			texts = append(texts, t)
		}
	}
	return strings.Join(texts, "\n")
}

func appendFiles(out []File, parts []interface{}) []File {
	for _, p := range parts {
		raw, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		// v1 protobuf Part wraps its payload in a "root" oneof:
		// {root:{file:{uri:…}}} or {root:{url:…}}. Unwrap it so a rooted file
		// part is detected the same way joinTextParts unwraps {root:{text:…}}.
		rooted := false
		if root, ok := raw["root"].(map[string]interface{}); ok {
			raw = root
			rooted = true
		}
		v0 := false
		if k, ok := raw["kind"].(string); ok && k == "file" {
			v0 = true
		}
		if t, ok := raw["type"].(string); ok && t == "file" {
			v0 = true
		}
		// A root-unwrapped part carries no kind/type discriminator; a nested
		// "file" object is what marks it a file part.
		if rooted {
			if _, ok := raw["file"].(map[string]interface{}); ok {
				v0 = true
			}
		}
		v1URL, _ := raw["url"].(string)
		if !v0 && v1URL == "" {
			continue
		}
		var f File
		if v0 {
			file, _ := raw["file"].(map[string]interface{})
			if file == nil {
				file = raw
			}
			uri, _ := file["uri"].(string)
			if uri == "" {
				continue
			}
			f.URI = uri
			if name, _ := file["name"].(string); name != "" {
				f.Name = name
			} else {
				f.Name = basename(uri)
			}
			if mt, ok := file["mimeType"].(string); ok {
				f.MimeType = mt
			}
			if sz, ok := numericSize(file["size"]); ok {
				f.Size = &sz
			}
		} else {
			f.URI = v1URL
			if name, _ := raw["filename"].(string); name != "" {
				f.Name = name
			} else {
				f.Name = basename(v1URL)
			}
			if mt, ok := raw["mediaType"].(string); ok {
				f.MimeType = mt
			}
		}
		out = append(out, f)
	}
	return out
}

// basename derives a display filename from a URI, mirroring the canvas
// basename() semantics: strip the workspace:/http(s):// scheme then take the
// path base, defaulting to "file" when nothing is left.
func basename(uri string) string {
	cleaned := strings.TrimPrefix(uri, "workspace:")
	cleaned = strings.TrimPrefix(cleaned, "https://")
	cleaned = strings.TrimPrefix(cleaned, "http://")
	if cleaned == "" {
		return "file"
	}
	return path.Base(cleaned)
}

func numericSize(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	}
	return 0, false
}
