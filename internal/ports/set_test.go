package ports

import "testing"

func TestParseSet(t *testing.T) {
	cases := []struct {
		spec, norm string
		count      int
		bad        bool
	}{
		{"", "", 0, false},
		{"22", "22", 1, false},
		{"8006, 22，3000-3100", "22,3000-3100,8006", 103, false},
		{"10000-20000 15000-25000", "10000-25000", 15001, false},
		{"5-9,10,11-12", "5-12", 8, false},   // 相邻合并
		{"1-65535", "1-65535", 65535, false}, // 节点可分配范围不受单次分配上限限制
		{"3100-3000", "", 0, true},
		{"0", "", 0, true},
		{"22,abc", "", 0, true},
		{"10-", "", 0, true},
	}
	for _, c := range cases {
		s, err := ParseSet(c.spec)
		if (err != nil) != c.bad || (err == nil && (s.String() != c.norm || s.Count() != c.count)) {
			t.Errorf("ParseSet(%q) = %q %d %v", c.spec, s.String(), s.Count(), err)
		}
	}
	s := MustSet("22,3000-3100,8006")
	for p, want := range map[int]bool{21: false, 22: true, 23: false, 2999: false, 3000: true, 3050: true, 3100: true, 3101: false, 8006: true, 65535: false} {
		if s.Contains(p) != want {
			t.Errorf("Contains(%d) = %v", p, !want)
		}
	}
	if n := MustSet("1-100").CountExcept(MustSet("22,50-59")); n != 89 {
		t.Errorf("CountExcept = %d", n)
	}
	if got := FromList([]int{8006, 22, 23, 24}).String(); got != "22-24,8006" {
		t.Errorf("FromList = %q", got)
	}
	if !MustSet("").Empty() || MustSet("1").Empty() {
		t.Error("Empty")
	}
}
