//go:build linux

package supervisor

import "golang.org/x/sys/unix"

// peerUID returns the effective uid of the socket peer via SO_PEERCRED.
func peerUID(fd uintptr) (int, error) {
	cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return int(cred.Uid), nil
}
