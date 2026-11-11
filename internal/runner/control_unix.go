//go:build !windows

package runner

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func controlAddress(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), controlSocketName)
}

type unixControlListener struct{ l *net.UnixListener }

func (u unixControlListener) Accept() (io.ReadWriteCloser, error) { return u.l.Accept() }

// Close also removes the socket file (the listener created it).
func (u unixControlListener) Close() error { return u.l.Close() }

// listenControl listens on <config dir>/control.sock with mode 0600. A
// socket left by a runner that died is removed, but only when nothing
// answers on it; anything else in the way is left alone.
func listenControl(cfgPath string) (controlListener, error) {
	path := controlAddress(cfgPath)
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("control channel: %s exists and isn't a socket", path)
		}
		c, derr := net.DialTimeout("unix", path, 2*time.Second)
		switch {
		case derr == nil:
			_ = c.Close()
			return nil, errControlInUse
		case errors.Is(derr, syscall.ECONNREFUSED):
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("control channel: remove the stale socket: %w", err)
			}
		case errors.Is(derr, os.ErrNotExist): // gone meanwhile
		default:
			return nil, fmt.Errorf("control channel: %s is in use (%v)", path, derr)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		if errors.Is(err, syscall.EINVAL) {
			return nil, fmt.Errorf("control channel: the socket path is too long for this system (%s): %w", path, err)
		}
		return nil, fmt.Errorf("control channel: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("control channel: %w", err)
	}
	return unixControlListener{l}, nil
}

func dialControl(cfgPath string) (io.ReadWriteCloser, error) {
	c, err := net.DialTimeout("unix", controlAddress(cfgPath), 2*time.Second)
	if err != nil {
		// EINVAL: the path is too long for a socket, so no runner listens there either
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOTSOCK) ||
			errors.Is(err, syscall.EINVAL) {
			return nil, ErrNotRunning
		}
		return nil, fmt.Errorf("control channel: %w", err)
	}
	return c, nil
}
