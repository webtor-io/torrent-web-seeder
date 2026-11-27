package uploads

import "testing"

// The vectors are shared by every repo that has these predicates.
func TestPredicates(t *testing.T) {
	cases := []struct {
		id                string
		homeHosted, owned bool
	}{
		{"9c5db5752b7c0123456789abcdef0123456789ab", true, true},
		{"9c5db5752b7c1123456789abcdef0123456789ab", true, false},
		{"9c5db5752b7cf123456789abcdef0123456789ab", true, false},
		{"9C5DB5752B7C0123456789ABCDEF0123456789AB", false, false},
		{"9C5DB5752B7C1123456789ABCDEF0123456789AB", false, false},
		{"9C5DB5752B7CF123456789ABCDEF0123456789AB", false, false},
		{"9c5db5752b7cA123456789abcdef0123456789ab", false, false},
		{"9c5db5752b7c/../x", false, false},
		{"9c5db5752b7c0123456789abcdef0123456789a", false, false},
		{"0123456789abcdef0123456789abcdef01234567", false, false},
	}
	for _, c := range cases {
		if HomeHosted(c.id) != c.homeHosted || Owned(c.id) != c.owned {
			t.Errorf("%q: HomeHosted %v Owned %v, want %v %v", c.id, HomeHosted(c.id), Owned(c.id), c.homeHosted, c.owned)
		}
	}
}
