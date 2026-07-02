//go:build !linux && !darwin

package supervisor

import "errors"

// peerUID fails closed on platforms without a supported peer-credential syscall:
// an RCE-equivalent API must never accept unauthenticated connections.
func peerUID(fd uintptr) (int, error) {
	return 0, errors.New("peer credential verification is unsupported on this platform")
}
