//go:build linux

package localcache

import (
	"fmt"
	"os"
	"syscall"
)

// publish bind-mounts dir onto at inside this mount namespace. With the symlink root
// mounted rshared (as the FUSE mount already requires) the bind propagates to the
// host and every peer container. Idempotent: if `at` already is a mount of the same
// device+inode as dir, nothing happens. Never unmounted (docs/28 §4.4).
func publish(dir, at string) error {
	if err := os.MkdirAll(at, 0o755); err != nil { //nolint:gosec // world-readable mountpoint
		return fmt.Errorf("localcache: mkdir publish_at %s: %w", at, err)
	}
	var sd, sa syscall.Stat_t
	if err := syscall.Stat(dir, &sd); err != nil {
		return fmt.Errorf("localcache: stat dir %s: %w", dir, err)
	}
	if err := syscall.Stat(at, &sa); err == nil && sd.Dev == sa.Dev && sd.Ino == sa.Ino {
		return nil // already published
	}
	if err := syscall.Mount(dir, at, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("localcache: bind-mount %s -> %s (needs CAP_SYS_ADMIN, like the FUSE mount): %w", dir, at, err)
	}
	return nil
}
