//go:build !darwin

package postproc

import "errors"

// cloneFile is unsupported off macOS; publishing copies bytes instead.
func cloneFile(src, dst string) error { return errors.ErrUnsupported }
