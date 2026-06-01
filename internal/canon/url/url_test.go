package url

import "testing"

func TestTemplate(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/score/8412", "/score/{id}"},
		{"/loan-application", "/loan-application"},
		{"/loan-application/8412/status", "/loan-application/{id}/status"},
		{"/charge/req_9f2", "/charge/req_9f2"}, // not a recognized id shape — left alone
		{"/users/3f2a4b1c-1111-2222-3333-444455556666/profile", "/users/{id}/profile"},
		{"/score/{id}", "/score/{id}"}, // already templated
		{"http://credit-bureau/score/8412", "/score/{id}"},
		{"/score/8412?trace=1", "/score/{id}"},
	}
	for _, c := range cases {
		if got := Template(c.in); got != c.want {
			t.Errorf("Template(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTemplateDeterministic(t *testing.T) {
	// Different ids in the same shape yield the same template.
	if Template("/score/1") != Template("/score/999999") {
		t.Error("numeric segments must collapse to the same template")
	}
}
