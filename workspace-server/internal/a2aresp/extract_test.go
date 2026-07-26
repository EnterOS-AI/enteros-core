package a2aresp

import "testing"

// TestText_ShapeMatrix pins the FULL A2A message/send result-shape contract in
// one table so the next unhandled shape fails HERE (in CI) instead of live in a
// dropped greeting. Coverage used to be reactive — the Task shape got a handler
// only after a 2026-07-19 live drop, the bare-Message shape only after a
// 2026-07-26 live drop. This enumerates every shape up front.
func TestText_ShapeMatrix(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		// The four result shapes, single text part.
		{"bare message (result.parts)",
			`{"jsonrpc":"2.0","result":{"kind":"message","parts":[{"kind":"text","text":"hi"}]}}`, "hi"},
		{"nested message (result.message.parts)",
			`{"result":{"message":{"parts":[{"kind":"text","text":"hi"}]}}}`, "hi"},
		{"task status (result.status.message.parts)",
			`{"result":{"kind":"task","status":{"message":{"parts":[{"kind":"text","text":"hi"}]}}}}`, "hi"},
		{"artifacts (result.artifacts[].parts)",
			`{"result":{"artifacts":[{"parts":[{"kind":"text","text":"hi"}]}]}}`, "hi"},

		// Robustness: text is NOT parts[0] — a reply led by a non-text part.
		{"leading file part then text",
			`{"result":{"parts":[{"kind":"file","file":{"uri":"x"}},{"kind":"text","text":"after file"}]}}`, "after file"},
		{"discriminated non-text part with a text field is skipped",
			`{"result":{"parts":[{"kind":"reasoning","text":"thinking..."},{"kind":"text","text":"answer"}]}}`, "answer"},
		{"text in the SECOND artifact",
			`{"result":{"artifacts":[{"parts":[{"kind":"file","file":{"uri":"x"}}]},{"parts":[{"kind":"text","text":"second art"}]}]}}`, "second art"},

		// v0.2 spelling: type=text instead of kind=text.
		{"v0.2 type=text spelling",
			`{"result":{"parts":[{"type":"text","text":"v02"}]}}`, "v02"},
		{"no discriminator but text field present",
			`{"result":{"parts":[{"text":"bare text"}]}}`, "bare text"},

		// The v1 protobuf {root:{text}} part. (result-as-string is AllText-only —
		// see TestAllText — Text no longer handles it.)
		{"v1 protobuf root.text part",
			`{"result":{"parts":[{"root":{"text":"rooted"}}]}}`, "rooted"},

		// Within a single shape, multiple text parts join with "\n".
		{"multiple text parts join with newline",
			`{"result":{"parts":[{"kind":"text","text":"a"},{"kind":"text","text":"b"}]}}`, "a\nb"},

		// SHAPE PRECEDENCE (the first shape wins — no cross-shape gluing).
		{"parts precede status.message and artifacts (no gluing)",
			`{"result":{"parts":[{"kind":"text","text":"P"}],"status":{"message":{"parts":[{"kind":"text","text":"S"}]}},"artifacts":[{"parts":[{"kind":"text","text":"A"}]}]}}`, "P"},
		{"artifacts precede status.message (finding #0 — interim status not glued to answer)",
			`{"result":{"status":{"message":{"parts":[{"kind":"text","text":"working"}]}},"artifacts":[{"parts":[{"kind":"text","text":"answer"}]}]}}`, "answer"},

		// Pre-unwrapped body (no jsonrpc envelope).
		{"pre-unwrapped bare task object",
			`{"kind":"message","parts":[{"kind":"text","text":"unwrapped"}]}`, "unwrapped"},

		// Empty / absent.
		{"empty parts array", `{"result":{"parts":[]}}`, ""},
		{"no result", `{"jsonrpc":"2.0","id":"1"}`, ""},
		{"only a file part, no text", `{"result":{"parts":[{"kind":"file","file":{"uri":"x"}}]}}`, ""},
		{"invalid json", `not json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Text([]byte(tc.body)); got != tc.want {
				t.Errorf("Text(%s)\n  got  %q\n  want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestAllText pins the full-history concatenation contract: AllText joins text
// from EVERY shape with "\n" (the persisted chat-history path uses this so a
// reply split across summary-in-parts + details-in-artifacts is captured whole),
// and it alone handles the {"result":"<string>"} bare-string shape.
func TestAllText(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"result as a plain string", `{"result":"just a string"}`, "just a string"},
		{"single shape", `{"result":{"parts":[{"kind":"text","text":"hi"}]}}`, "hi"},
		{"status.message + artifacts concatenate",
			`{"result":{"status":{"message":{"parts":[{"kind":"text","text":"working"}]}},"artifacts":[{"parts":[{"kind":"text","text":"answer"}]}]}}`, "working\nanswer"},
		{"parts + status.message + message + artifacts all join",
			`{"result":{"parts":[{"kind":"text","text":"P"}],"status":{"message":{"parts":[{"kind":"text","text":"S"}]}},"message":{"parts":[{"kind":"text","text":"M"}]},"artifacts":[{"parts":[{"kind":"text","text":"A"}]}]}}`, "P\nS\nM\nA"},
		{"no result", `{"jsonrpc":"2.0","id":"1"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AllText([]byte(tc.body)); got != tc.want {
				t.Errorf("AllText(%s)\n  got  %q\n  want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestFiles_Order pins attachment display order: files in artifacts precede
// files in message (matching the pre-consolidation extractFilesFromTask walk
// parts → artifacts → status.message → message).
func TestFiles_Order(t *testing.T) {
	body := `{"result":{"artifacts":[{"parts":[{"kind":"file","file":{"uri":"a://art","name":"art"}}]}],"message":{"parts":[{"kind":"file","file":{"uri":"a://msg","name":"msg"}}]}}}`
	got := Files([]byte(body))
	if len(got) != 2 {
		t.Fatalf("Files count: got %d want 2 (%+v)", len(got), got)
	}
	if got[0].Name != "art" || got[1].Name != "msg" {
		t.Errorf("Files order: got [%q, %q], want [\"art\", \"msg\"] (artifacts before message)", got[0].Name, got[1].Name)
	}
}

func TestBasename(t *testing.T) {
	// Mirrors canvas basename() semantics (moved here from messagestore during
	// the SSOT consolidation).
	cases := []struct{ in, want string }{
		{"workspace:/uploads/shot.png", "shot.png"},
		{"workspace:/a/b/c/file.txt", "file.txt"},
		{"https://example.com/path/file.csv", "file.csv"},
		{"http://x/y", "y"},
		{"", "file"},
		{"workspace:", "file"},
	}
	for _, tc := range cases {
		if got := basename(tc.in); got != tc.want {
			t.Errorf("basename(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestErrorMessage(t *testing.T) {
	if got := ErrorMessage([]byte(`{"error":{"message":"workspace not found"}}`)); got != "workspace not found" {
		t.Errorf("error message: got %q", got)
	}
	if got := ErrorMessage([]byte(`{"result":{"parts":[{"kind":"text","text":"ok"}]}}`)); got != "" {
		t.Errorf("non-error should return empty: got %q", got)
	}
}

func TestFiles_ShapeMatrix(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantURI   string
		wantName  string
		wantMime  string
		wantCount int
	}{
		{"v0 file in bare parts",
			`{"result":{"parts":[{"kind":"file","file":{"uri":"s3://b/a.png","name":"a.png","mimeType":"image/png"}}]}}`,
			"s3://b/a.png", "a.png", "image/png", 1},
		{"v0 file, name derived from uri basename",
			`{"result":{"parts":[{"kind":"file","file":{"uri":"https://x/y/doc.pdf","mimeType":"application/pdf"}}]}}`,
			"https://x/y/doc.pdf", "doc.pdf", "application/pdf", 1},
		{"v1 protobuf-flat file (top-level url/filename/mediaType)",
			`{"result":{"parts":[{"url":"https://x/z.jpg","filename":"z.jpg","mediaType":"image/jpeg"}]}}`,
			"https://x/z.jpg", "z.jpg", "image/jpeg", 1},
		{"file inside artifacts",
			`{"result":{"artifacts":[{"parts":[{"kind":"file","file":{"uri":"a://f","name":"f"}}]}]}}`,
			"a://f", "f", "", 1},
		{"file inside status.message",
			`{"result":{"status":{"message":{"parts":[{"kind":"file","file":{"uri":"a://s","name":"s"}}]}}}}`,
			"a://s", "s", "", 1},
		{"v1 protobuf root.file part",
			`{"result":{"parts":[{"root":{"file":{"uri":"workspace:/r.pdf","name":"r.pdf"}}}]}}`,
			"workspace:/r.pdf", "r.pdf", "", 1},
		{"text-only part yields no files", `{"result":{"parts":[{"kind":"text","text":"hi"}]}}`, "", "", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Files([]byte(tc.body))
			if len(got) != tc.wantCount {
				t.Fatalf("Files count: got %d want %d (%+v)", len(got), tc.wantCount, got)
			}
			if tc.wantCount == 1 {
				if got[0].URI != tc.wantURI || got[0].Name != tc.wantName || got[0].MimeType != tc.wantMime {
					t.Errorf("Files[0] = %+v, want uri=%q name=%q mime=%q", got[0], tc.wantURI, tc.wantName, tc.wantMime)
				}
			}
		})
	}
}
