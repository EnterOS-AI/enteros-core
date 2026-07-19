package wirepath

import "testing"

// These assertions run IDENTICALLY on every OS — that is the point of the
// package. A regression that reintroduces filepath.ToSlash (no-op on Linux)
// or filepath.Join (host separator) fails this suite on the Linux per-PR CI,
// not just on a Windows dev machine.
func TestNormalize(t *testing.T) {
	cases := map[string]string{
		`win\style\file.txt`:      "win/style/file.txt",
		`configs\plugins\.staging`: "configs/plugins/.staging",
		"already/slash":           "already/slash",
		`mixed\and/slash`:         "mixed/and/slash",
		"a/../b.txt":              "b.txt",
		`a\..\b.txt`:              "b.txt",
		"./x":                     "x",
		"/configs/x":              "/configs/x",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestJoin(t *testing.T) {
	cases := []struct {
		elems []string
		want  string
	}{
		{[]string{"/configs", `nested\dir\f.txt`}, "/configs/nested/dir/f.txt"},
		{[]string{"prefix", "rel"}, "prefix/rel"},
		{[]string{`p\a`, `b\c`}, "p/a/b/c"},
		{[]string{"/configs", "..", "escape"}, "/escape"},
	}
	for _, c := range cases {
		if got := Join(c.elems...); got != c.want {
			t.Errorf("Join(%v) = %q; want %q", c.elems, got, c.want)
		}
	}
}
