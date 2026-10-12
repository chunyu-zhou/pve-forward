package ports

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		kind, spec, norm string
		n                int
		bad              bool
	}{
		{"single", " 10022 ", "10022", 1, false},
		{"single", "0", "", 0, true},
		{"single", "70000", "", 0, true},
		{"multi", "8002，8001, 8002 8005", "8001,8002,8005", 3, false},
		{"multi", "", "", 0, true},
		{"multi", "1,abc", "", 0, true},
		{"range", "20000-20009", "20000-20009", 10, false},
		{"range", "20009-20000", "", 0, true},
		{"range", "1-65535", "", 0, true},
		{"range", "20000", "", 0, true},
		{"x", "1", "", 0, true},
	}
	for _, c := range cases {
		norm, ps, err := Parse(c.kind, c.spec)
		if (err != nil) != c.bad || norm != c.norm || len(ps) != c.n {
			t.Errorf("Parse(%s,%q) = %q %d %v", c.kind, c.spec, norm, len(ps), err)
		}
	}
}
