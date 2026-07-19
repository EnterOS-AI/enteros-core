// Package wirepath normalizes host-derived relative paths into the
// slash-separated form used "on the wire": tar entry names, in-container
// paths, and plugin/config keys that a Linux daemon will interpret.
//
// Normalization is UNCONDITIONAL — backslashes become forward slashes on
// every OS, not just Windows. filepath.ToSlash is a no-op on Linux, which
// made the Windows-host behavior untestable on the Linux per-PR CI (the
// 2026-07-19 backslash-tar incident class could regress invisibly). The
// platform's paths are always produced by our own code from relative
// segments, so a literal backslash can only mean "Windows separator that
// leaked through" — normalizing it uniformly makes one CI cover every
// platform. (The pathological case — a Linux file whose NAME contains a
// backslash — is deliberately renamed rather than allowed to corrupt the
// wire format.)
package wirepath

import (
	"path"
	"strings"
)

// Normalize converts every backslash to a forward slash and path.Cleans the
// result. Intended for RELATIVE wire paths (tar entry names, keys); absolute
// container paths pass through with the same separator guarantee.
func Normalize(p string) string {
	return path.Clean(strings.ReplaceAll(p, `\`, "/"))
}

// Join normalizes each element and joins with forward slashes (path.Join
// semantics, never the host separator).
func Join(elem ...string) string {
	for i, e := range elem {
		elem[i] = strings.ReplaceAll(e, `\`, "/")
	}
	return path.Join(elem...)
}
