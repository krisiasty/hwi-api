//go:build linux

package inventory

import "syscall"

// Uname reports the kernel's machine architecture, which is not necessarily
// the architecture of userspace.
func Uname() (string, error) {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return "", err
	}
	return charsToString(u.Machine[:]), nil
}

// charsToString converts a NUL-terminated Utsname field to a string. The
// element type differs by architecture, so the conversion goes through a
// generic constraint rather than a cast.
func charsToString[T int8 | uint8](ca []T) string {
	b := make([]byte, 0, len(ca))
	for _, c := range ca {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}
