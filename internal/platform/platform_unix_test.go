//go:build darwin || linux

package platform

import "testing"

// TestUnescapeMount covers the octal escapes Linux writes in
// /proc/self/mounts. A wrong decode makes a mounted path with a space read
// as unmounted, and mount then mounts it a second time.
func TestUnescapeMount(t *testing.T) {
	for in, want := range map[string]string{
		`/home/me/kDrive`:            "/home/me/kDrive",
		`/home/me/my\040drive`:       "/home/me/my drive",
		`/a\011b\012c`:               "/a\tb\nc",
		`/back\134slash`:             `/back\slash`,
		`/ends\040`:                  "/ends ",
		`/trailing\`:                 `/trailing\`,
		`/short\04`:                  `/short\04`,
		`/not\9octal`:                `/not\9octal`,
		`/two\040spaces\040here`:     "/two spaces here",
		`/unicode\303\244ra\303\266`: "/unicodeäraö",
	} {
		if got := unescapeMount(in); got != want {
			t.Errorf("unescapeMount(%q) = %q, want %q", in, got, want)
		}
	}
}
