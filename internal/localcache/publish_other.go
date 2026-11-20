//go:build !linux

package localcache

import "errors"

func publish(_, _ string) error {
	return errors.New("localcache: publish bind-mount is only supported on linux")
}
