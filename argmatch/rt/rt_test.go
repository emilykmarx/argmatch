package rt

import "testing"

func TestEscape(t *testing.T) {
	for _, test := range []struct{ in, want string }{
		{"/etc/prometheus/prometheus.yml", "/etc/prometheus/prometheus.yml"},
		{`say "hi"`, `say "hi"`},
		{"héllo, 世界", "héllo, 世界"},
		{"a\x00b", `a\x00b`},
		{"tab\tnewline\n", `tab\tnewline\n`},
		{`back\slash`, `back\\slash`},
		{`literal \x00`, `literal \\x00`},
		{"bad \xff utf8", `bad \xff utf8`},
		{"\x01job\x08__name__", `\x01job\b__name__`},
		{string(rune(0x200b)), `\` + "u200b"}, // zero-width space
	} {
		if got := escape(test.in); got != test.want {
			t.Errorf("escape(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}
