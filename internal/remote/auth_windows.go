//go:build windows

package remote

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// Windows OpenSSH's ssh-agent listens on this named pipe and does not set
// SSH_AUTH_SOCK, so ssh.exe finds it without an env var. Match that.
const windowsOpenSSHAgentPipe = `\\.\pipe\openssh-ssh-agent`

func defaultAgentSock() string { return windowsOpenSSHAgentPipe }

func dialAgentConn(sock string) (net.Conn, error) {
	if c, err := net.DialTimeout("unix", sock, agentDialTimeout); err == nil {
		return c, nil
	}
	pipe := sock
	if !isWindowsNamedPipe(sock) {
		pipe = windowsOpenSSHAgentPipe
	}
	return dialNamedPipe(pipe)
}

// isWindowsNamedPipe reports whether sock is a named pipe rather than a TCP
// address, and picks the pipe to dial. The prefix is a Windows API literal,
// so the compare folds with core.FoldASCII: strings.ToLower also folds runes
// whose lowercase form is ASCII, so a pipe path spelled with U+212A would
// reach the named-pipe dial as one Windows does not name.
func isWindowsNamedPipe(p string) bool {
	p = strings.ReplaceAll(p, `/`, `\`)
	return strings.HasPrefix(core.FoldASCII(p), `\\.\pipe\`)
}

func dialNamedPipe(path string) (net.Conn, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// FILE_FLAG_OVERLAPPED carries the whole reason this file exists. Without
	// it CreateFile on a client named pipe blocks in the kernel until the
	// agent accepts, and nothing the caller holds can cancel that: an agent
	// that is not running, or one that never calls ConnectNamedPipe, hangs
	// Connect for good. It also decides what os.NewFile can do with the
	// handle. A synchronous one cannot join the runtime poller, so every
	// later read is uninterruptible and SetDeadline answers os.ErrNoDeadline
	// instead of arming anything. With the flag the open returns at once and
	// the connection completes on the first overlapped read, which is where
	// the bound below applies.
	h, err := syscall.CreateFile(name,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, nil, syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL|syscall.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), path)
	if f == nil {
		syscall.CloseHandle(h)
		return nil, fmt.Errorf("cannot use the agent pipe %s", path)
	}
	// The connect is the first read: the agent protocol is request then
	// reply, so nothing is readable until it has answered. Arming the read
	// deadline here is the bounded wait the unix dial gets from
	// net.DialTimeout; pipeConn.Read drops it once the wait is over.
	if err := f.SetReadDeadline(time.Now().Add(agentDialTimeout)); err != nil {
		f.Close()
		return nil, err
	}
	return &pipeConn{File: f}, nil
}

type pipeConn struct {
	*os.File
	// armed is set while the read deadline dialNamedPipe installed is still
	// bounding the connect. It is cleared by the first read that completes,
	// whether it succeeded or not: a bound left armed after the agent has
	// answered would cut the session off mid-conversation.
	armed sync.Once
}

func (c *pipeConn) Read(p []byte) (int, error) {
	n, err := c.File.Read(p)
	c.armed.Do(func() { _ = c.File.SetReadDeadline(time.Time{}) })
	return n, err
}

func (c *pipeConn) LocalAddr() net.Addr                { return pipeAddr(c.Name()) }
func (c *pipeConn) RemoteAddr() net.Addr               { return pipeAddr(c.Name()) }
func (c *pipeConn) SetDeadline(t time.Time) error      { return c.File.SetDeadline(t) }
func (c *pipeConn) SetReadDeadline(t time.Time) error  { return c.File.SetReadDeadline(t) }
func (c *pipeConn) SetWriteDeadline(t time.Time) error { return c.File.SetWriteDeadline(t) }

type pipeAddr string

func (pipeAddr) Network() string  { return "pipe" }
func (a pipeAddr) String() string { return string(a) }
