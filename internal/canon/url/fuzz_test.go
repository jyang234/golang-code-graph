package url

import (
	"strings"
	"testing"
)

// FuzzTemplateInvariants pins the route canonicalizer's structural guarantees on
// arbitrary input: Template never panics, and its output is a FIXED POINT
// (re-templating a template yields the same string) carrying none of the volatile
// URL parts the canonicalizer promises to strip — no scheme ("://"), query
// ("?"), or fragment ("#"). Idempotence is what lets a route key be stable no
// matter how many normalization passes it survives.
func FuzzTemplateInvariants(f *testing.F) {
	for _, s := range []string{
		"", "/", "/score/8412", "/loans/3f2a4b6c-1111-2222-3333-444455556666",
		"https://host/score/1?q=2#frag", "/a//b", "/a/b/", "/a/b",
		"a://b://c", "/x:y/z", "/{id}/edit", "//", "/café/北京", "deadbeefdeadbeef",
		":///://0", // regression: residual scheme after one strip (FuzzTemplateInvariants found this)
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, path string) {
		got := Template(path)
		if again := Template(got); again != got {
			t.Errorf("Template not idempotent:\n in:   %q\n once: %q\n twice:%q", path, got, again)
		}
		for _, bad := range []string{"://", "?", "#"} {
			if strings.Contains(got, bad) {
				t.Errorf("Template(%q) = %q still contains %q (volatile URL part not stripped)", path, got, bad)
			}
		}
	})
}
