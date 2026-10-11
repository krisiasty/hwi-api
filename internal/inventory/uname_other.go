//go:build !linux

package inventory

import "errors"

// Uname is unavailable off Linux. The agent only ships for Linux; this
// exists so the package builds and unit-tests on a developer's machine.
func Uname() (string, error) { return "", errors.New("uname: unsupported on this platform") }
