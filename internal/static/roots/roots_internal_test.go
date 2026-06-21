package roots

import "testing"

// TestHasPrimaryRoot pins the library-fallback gate after init() became a root.
// Every package has a synthesized init, so the fallback can no longer key on
// "len(roots)==0"; it keys on the presence of a PRIMARY root (main/http/consumer).
// Init and export roots must NOT count — counting init would permanently suppress
// the fallback, counting export would make the gate test for the very thing it
// gates. The "only inits" case is the exact regression this guards.
func TestHasPrimaryRoot(t *testing.T) {
	cases := []struct {
		name  string
		kinds []Kind
		want  bool
	}{
		{"empty", nil, false},
		{"only inits", []Kind{KindInit, KindInit}, false},
		{"inits do not unlock fallback", []Kind{KindInit, KindExport}, false},
		{"main", []Kind{KindInit, KindMain}, true},
		{"http handler", []Kind{KindInit, KindHTTP}, true},
		{"bus consumer", []Kind{KindInit, KindConsumer}, true},
		// Declared callback/worker roots are author-asserted real entries, so they
		// unlock the fallback the same way a discovered primary does — a service whose
		// only entry is a declared one must not collapse into library-export mode.
		{"declared callback", []Kind{KindInit, KindCallback}, true},
		{"declared worker", []Kind{KindInit, KindWorker}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var rs []Root
			for _, k := range c.kinds {
				rs = append(rs, Root{Kind: k})
			}
			if got := hasPrimaryRoot(rs); got != c.want {
				t.Errorf("hasPrimaryRoot(%v) = %v, want %v", c.kinds, got, c.want)
			}
		})
	}
}
