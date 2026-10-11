//go:build !linux

package ethtool

import (
	"context"
	"errors"
)

// Dial fails everywhere but Linux: ethtool netlink is a Linux kernel interface.
func Dial(context.Context) (*Client, error) {
	return nil, errors.New("ethtool netlink is Linux only")
}
