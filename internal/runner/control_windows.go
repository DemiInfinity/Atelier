package runner

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The control channel on Windows: the named pipe \\.\pipe\atelier-runner
// (docs/app-api.md, section 10), open to the user the runner runs as (and
// SYSTEM and Administrators), local clients only.
//
// The pipe handles are synchronous; timeouts are done by closing the
// connection, which cancels the pending read or write (CancelIoEx).

const controlPipeName = `\\.\pipe\atelier-runner`

func controlAddress(string) string { return controlPipeName }

type pipeConn struct {
	h    windows.Handle
	mu   sync.Mutex
	done bool
}

func (c *pipeConn) closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.done
}

func (c *pipeConn) Read(p []byte) (int, error) {
	if c.closed() {
		return 0, net.ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	var n uint32
	err := windows.ReadFile(c.h, p, &n, nil)
	switch {
	case err == nil && n == 0:
		return 0, io.EOF
	case errors.Is(err, windows.ERROR_BROKEN_PIPE), errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED), errors.Is(err, windows.ERROR_NO_DATA):
		return int(n), io.EOF
	case err != nil && n > 0:
		return int(n), nil
	}
	return int(n), err
}

func (c *pipeConn) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		if c.closed() {
			return total, net.ErrClosed
		}
		var n uint32
		if err := windows.WriteFile(c.h, p, &n, nil); err != nil {
			return total + int(n), err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
		total += int(n)
		p = p[n:]
	}
	return total, nil
}

// Close cancels pending I/O on the handle and closes it. Data already
// written stays readable by the other end.
func (c *pipeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return nil
	}
	c.done = true
	_ = windows.CancelIoEx(c.h, nil)
	return windows.CloseHandle(c.h)
}

type pipeListener struct {
	sa *windows.SecurityAttributes

	mu     sync.Mutex
	next   windows.Handle // the instance waiting for a client
	closed bool
}

// controlPipeSecurity lets only this user, SYSTEM and Administrators open the pipe.
func controlPipeSecurity() (*windows.SecurityAttributes, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sddl := "D:P(A;;GA;;;" + tu.User.Sid.String() + ")(A;;GA;;;SY)(A;;GA;;;BA)"
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}, nil
}

func (l *pipeListener) newInstance(first bool) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(controlPipeName)
	if err != nil {
		return 0, err
	}
	flags := uint32(windows.PIPE_ACCESS_DUPLEX)
	if first {
		// fails when the pipe already exists: another runner, or someone squatting on the name
		flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	mode := uint32(windows.PIPE_TYPE_BYTE | windows.PIPE_READMODE_BYTE | windows.PIPE_WAIT | windows.PIPE_REJECT_REMOTE_CLIENTS)
	return windows.CreateNamedPipe(name, flags, mode, windows.PIPE_UNLIMITED_INSTANCES, 64<<10, 64<<10, 0, l.sa)
}

// listenControl creates the pipe. A pipe disappears with its process, so
// there is no stale one to clean up: if it exists, a runner is running.
func listenControl(string) (controlListener, error) {
	sa, err := controlPipeSecurity()
	if err != nil {
		return nil, fmt.Errorf("control channel: %w", err)
	}
	l := &pipeListener{sa: sa}
	h, err := l.newInstance(true)
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, errControlInUse
		}
		return nil, fmt.Errorf("control channel: %w", err)
	}
	l.next = h
	return l, nil
}

func (l *pipeListener) Accept() (io.ReadWriteCloser, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, net.ErrClosed
	}
	h := l.next
	l.mu.Unlock()
	if h == 0 {
		var err error
		if h, err = l.newInstance(false); err != nil {
			return nil, err
		}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			_ = windows.CloseHandle(h)
			return nil, net.ErrClosed
		}
		l.next = h
		l.mu.Unlock()
	}
	err := windows.ConnectNamedPipe(h, nil) // blocks until a client connects (Close connects one)
	l.mu.Lock()
	l.next = 0
	closed := l.closed
	l.mu.Unlock()
	if closed {
		_ = windows.CloseHandle(h)
		return nil, net.ErrClosed
	}
	// Have the next instance ready before this client is served, so the
	// pipe never disappears between clients (a failure is retried by the
	// next Accept).
	if nh, nerr := l.newInstance(false); nerr == nil {
		l.mu.Lock()
		if l.closed {
			_ = windows.CloseHandle(nh)
		} else {
			l.next = nh
		}
		l.mu.Unlock()
	}
	if err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) && !errors.Is(err, windows.ERROR_NO_DATA) {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	return &pipeConn{h: h}, nil
}

// Close stops Accept. A blocked ConnectNamedPipe is woken by cancelling it
// and, should that be too early, by connecting to the pipe ourselves.
func (l *pipeListener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	h := l.next
	l.mu.Unlock()
	if h != 0 {
		_ = windows.CancelIoEx(h, nil)
		if c, err := openControlPipe(); err == nil {
			_ = windows.CloseHandle(c)
		}
	}
	return nil
}

func openControlPipe() (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(controlPipeName)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
}

func dialControl(string) (io.ReadWriteCloser, error) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		h, err := openControlPipe()
		switch {
		case err == nil:
			return &pipeConn{h: h}, nil
		case errors.Is(err, windows.ERROR_FILE_NOT_FOUND), errors.Is(err, windows.ERROR_PATH_NOT_FOUND):
			return nil, ErrNotRunning
		case errors.Is(err, windows.ERROR_PIPE_BUSY) && time.Now().Before(deadline):
			// every instance is taken for a moment: the server makes a new one per client
			time.Sleep(25 * time.Millisecond)
		default:
			return nil, fmt.Errorf("control channel: %w", err)
		}
	}
}
