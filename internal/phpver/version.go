// Package phpver knows how to parse PHP version strings, resolve them
// against windows.php.net releases, and install builds locally.
package phpver

import (
	"fmt"
	"strconv"
	"strings"
)

// Selector is a possibly-partial version the user typed: "8", "8.3" or "8.3.26".
type Selector struct {
	Major, Minor, Patch int // -1 means unspecified
}

func ParseSelector(s string) (Selector, error) {
	sel := Selector{Major: -1, Minor: -1, Patch: -1}
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) == 0 || len(parts) > 3 {
		return sel, fmt.Errorf("invalid version %q", s)
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return sel, fmt.Errorf("invalid version %q", s)
		}
		nums[i] = n
	}
	sel.Major = nums[0]
	if len(nums) > 1 {
		sel.Minor = nums[1]
	}
	if len(nums) > 2 {
		sel.Patch = nums[2]
	}
	return sel, nil
}

// Matches reports whether a full version like "8.3.26" satisfies the selector.
func (sel Selector) Matches(full string) bool {
	v, err := ParseSelector(full)
	if err != nil || v.Patch == -1 {
		return false
	}
	if sel.Major != v.Major {
		return false
	}
	if sel.Minor != -1 && sel.Minor != v.Minor {
		return false
	}
	if sel.Patch != -1 && sel.Patch != v.Patch {
		return false
	}
	return true
}

func (sel Selector) String() string {
	s := strconv.Itoa(sel.Major)
	if sel.Minor != -1 {
		s += "." + strconv.Itoa(sel.Minor)
	}
	if sel.Patch != -1 {
		s += "." + strconv.Itoa(sel.Patch)
	}
	return s
}

// Compare orders two full versions ("8.3.2" < "8.3.10").
func Compare(a, b string) int {
	av, _ := ParseSelector(a)
	bv, _ := ParseSelector(b)
	for _, pair := range [][2]int{{av.Major, bv.Major}, {av.Minor, bv.Minor}, {av.Patch, bv.Patch}} {
		if pair[0] != pair[1] {
			if pair[0] < pair[1] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// FcgiPort maps a version's major.minor to a stable local port:
// 8.3 -> 9083, 8.2 -> 9082, 7.4 -> 9074.
func FcgiPort(full string) (int, error) {
	v, err := ParseSelector(full)
	if err != nil || v.Minor == -1 {
		return 0, fmt.Errorf("invalid version %q", full)
	}
	return 9000 + v.Major*10 + v.Minor, nil
}
