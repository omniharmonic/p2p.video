//go:build darwin

package postproc

import "golang.org/x/sys/unix"

// cloneFile makes dst an APFS copy-on-write clone of src: constant time and
// no extra disk space however large the media is. Callers fall back to a
// byte copy when cloning is unsupported (other volume or file system).
func cloneFile(src, dst string) error {
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}
