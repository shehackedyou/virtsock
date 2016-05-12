package hvsock

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"

	"encoding/binary"
)

// This package provides a Go interface to Hyper-V sockets both on
// Windows and on Linux (assuming the appropriate Linux kernel patches
// have been applied).
//
// Unfortunately, it is not easy/possible to extend the existing Go
// socket implementations with new Address Families, so this module
// wraps directly around system calls (and handles Windows'
// asynchronous system calls).
//
// There is an additional wrinkle. Hyper-V sockets in currently
// shipping versions of Windows don't support graceful and/or
// unidirectional shutdown(). So we turn a stream based protocol into
// message based protocol which allows to send in-line "messages" to
// the other end. We then provide a stream based interface on top of
// that. Yuk.
//
// The message interface is pretty simple. We first send a 32bit
// message containing the size of the data in the following
// message. Messages are limited to 'maxmsgsize'. Special message
// (without data), `shutdownrd` and 'shutdownwr' are used to used to
// signal a shutdown to the other end.

const (
	maxMsgSize = 32 * 1024 // Maximum message size
)

// Hypper-V sockets use GUIDs for addresses and "ports"
type GUID [16]byte

// Convert a GUID into a string
func (g *GUID) String() string {
	/* XXX This assume little endian */
	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		g[3], g[2], g[1], g[0],
		g[5], g[4],
		g[7], g[6],
		g[8], g[9],
		g[10], g[11], g[12], g[13], g[14], g[15])
}

// Parse a GUID string
func GuidFromString(s string) (GUID, error) {
	var g GUID
	var err error
	_, err = fmt.Sscanf(s, "%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		&g[3], &g[2], &g[1], &g[0],
		&g[5], &g[4],
		&g[7], &g[6],
		&g[8], &g[9],
		&g[10], &g[11], &g[12], &g[13], &g[14], &g[15])
	return g, err
}

type HypervAddr struct {
	VmId      GUID
	ServiceId GUID
}

func (a HypervAddr) Network() string { return "hvsock" }

func (a HypervAddr) String() string {
	vmid := a.VmId.String()
	svc := a.ServiceId.String()

	return vmid + ":" + svc
}

var (
	GUID_ZERO, _      = GuidFromString("00000000-0000-0000-0000-000000000000")
	GUID_WILDCARD, _  = GuidFromString("00000000-0000-0000-0000-000000000000")
	GUID_BROADCAST, _ = GuidFromString("FFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF")
	GUID_CHILDREN, _  = GuidFromString("90db8b89-0d35-4f79-8ce9-49ea0ac8b7cd")
	GUID_LOOPBACK, _  = GuidFromString("e0e16197-dd56-4a10-9195-5ee7a155a838")
	GUID_PARENT, _    = GuidFromString("a42e7cda-d03f-480c-9cc2-a4de20abb878")
)

func Dial(raddr HypervAddr) (Conn, error) {
	fd, err := syscall.Socket(AF_HYPERV, syscall.SOCK_STREAM, SHV_PROTO_RAW)
	if err != nil {
		return nil, err
	}

	err = connect(fd, &raddr)
	if err != nil {
		return nil, err
	}

	v, err := newHVsockConn(fd, HypervAddr{VmId: GUID_ZERO, ServiceId: GUID_ZERO}, raddr)
	if err != nil {
		return nil, err
	}
	v.wrlock = &sync.Mutex{}
	return v, nil
}

func Listen(addr HypervAddr) (net.Listener, error) {

	accept_fd, err := syscall.Socket(AF_HYPERV, syscall.SOCK_STREAM, SHV_PROTO_RAW)
	if err != nil {
		return nil, err
	}

	err = bind(accept_fd, addr)
	if err != nil {
		return nil, err
	}

	err = syscall.Listen(accept_fd, syscall.SOMAXCONN)
	if err != nil {
		return nil, err
	}

	return &hvsockListener{accept_fd, addr}, nil
}

const (
	shutdownrd = 0xdeadbeef // Message for CloseRead()
	shutdownwr = 0xbeefdead // Message for CloseWrite()
	closemsg   = 0xdeaddead // Message for Close()
)

// Conn is a hvsock connection which support half-close.
type Conn interface {
	net.Conn
	CloseRead() error
	CloseWrite() error
}

func (v *hvsockListener) Accept() (net.Conn, error) {
	var raddr HypervAddr
	fd, err := accept(v.accept_fd, &raddr)
	if err != nil {
		return nil, err
	}

	a, err := newHVsockConn(fd, v.laddr, raddr)
	if err != nil {
		return nil, err
	}
	a.wrlock = &sync.Mutex{}
	return a, nil
}

func (v *hvsockListener) Close() error {
	// Note this won't cause the Accept to unblock.
	return syscall.Close(v.accept_fd)
}

func (v *hvsockListener) Addr() net.Addr {
	return HypervAddr{VmId: v.laddr.VmId, ServiceId: v.laddr.ServiceId}
}

/*
 * A wrapper around FileConn which supports CloseRead and CloseWrite
 */

var (
	errSocketClosed        = errors.New("HvSocket has already been closed")
	errSocketWriteClosed   = errors.New("HvSocket has been closed for write")
	errSocketReadClosed    = errors.New("HvSocket has been closed for read")
	errSocketMsgSize       = errors.New("HvSocket message was of wrong size")
	errSocketMsgWrite      = errors.New("HvSocket writing message")
	errSocketNotEnoughData = errors.New("HvSocket not enough data written")
	errSocketUnImplemented = errors.New("Function not implemented")
)

type HVsockConn struct {
	hvsockConn

	wrlock *sync.Mutex

	writeClosed bool
	readClosed  bool

	bytesToRead int
}

func (v *HVsockConn) LocalAddr() net.Addr {
	return v.local
}

func (v *HVsockConn) RemoteAddr() net.Addr {
	return v.remote
}

func (v *HVsockConn) Close() error {
	fmt.Printf("Close\n")

	v.readClosed = true
	v.writeClosed = true

	// Send close message
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, closemsg)
	v.wrlock.Lock()
	n, err := v.write(b)
	v.wrlock.Unlock()
	fmt.Printf("TX: Close\n")
	if err != nil {
		// chances are that the other end beat us to the close
		fmt.Printf("Mmmm. %s\n", err)
		return v.close()
	}
	if n != len(b) {
		v.close()
		return errSocketMsgWrite
	}

	// wait for reply/ignore errors
	// we may get a EOF because the other end  closed,
	_, _ = v.read(b)
	fmt.Printf("close\n")
	return v.close()
}

func (v *HVsockConn) CloseRead() error {
	if v.readClosed {
		return errSocketReadClosed
	}

	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, shutdownrd)
	v.wrlock.Lock()
	n, err := v.write(b)
	v.wrlock.Unlock()
	if err != nil {
		return err
	}
	if n != len(b) {
		return errSocketMsgWrite
	}

	v.readClosed = true
	return nil
}

func (v *HVsockConn) CloseWrite() error {
	if v.writeClosed {
		return errSocketWriteClosed
	}

	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, shutdownwr)
	v.wrlock.Lock()
	n, err := v.write(b)
	v.wrlock.Unlock()
	if err != nil {
		return err
	}
	if n != len(b) {
		return errSocketMsgWrite
	}

	v.writeClosed = true
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Read into buffer. This function turns a stream interface into
// messages and also handles the inband control messages.
func (v *HVsockConn) Read(buf []byte) (int, error) {
	if v.readClosed {
		return 0, io.EOF
	}

	if v.bytesToRead == 0 {
		for {
			// wait for next message
			b := make([]byte, 4)

			n, err := v.read(b)
			if err != nil {
				return 0, err
			}

			if n != 4 {
				return n, errSocketMsgSize
			}

			msg := int(binary.LittleEndian.Uint32(b))
			if msg == shutdownwr {
				// The other end shutdown write. No point reading more
				v.readClosed = true
				fmt.Printf("RX: ShutdownWrite\n")
				return 0, io.EOF
			} else if msg == shutdownrd {
				// The other end shutdown read. No point writing more
				v.writeClosed = true
				fmt.Printf("RX: ShutdownRead\n")
			} else if msg == closemsg {
				// Setting write close here forces a proper close
				v.writeClosed = true
				fmt.Printf("RX: Close\n")
				v.Close()
			} else {
				v.bytesToRead = msg
				break
			}
		}
	}

	// If we get here, we know there is v.bytesToRead worth of
	// data coming our way. Read it directly into to buffer passed
	// in by the caller making sure we do not read mode than we
	// should read by splicing the buffer.
	toRead := min(len(buf), v.bytesToRead)
	//fmt.Printf("READ:  %d len=0x%x\n", int(v.fd), toRead)
	n, err := v.read(buf[:toRead])
	if err != nil || n == 0 {
		v.readClosed = true
		return n, err
	}
	v.bytesToRead -= n
	return n, nil
}

func (v *HVsockConn) Write(buf []byte) (int, error) {
	if v.writeClosed {
		return 0, errSocketWriteClosed
	}

	b := make([]byte, 4)
	toWrite := len(buf)
	written := 0

	//fmt.Printf("WRITE: %d Total len=%x\n", int(v.fd), len(buf))
	//fmt.Printf("FD: %d\n", int(v.fd))

	for toWrite > 0 {
		// We write batches of MSG + data which need to be
		// "atomic". We don't want to hold the lock for the
		// entire Write() in case some other threads wants to
		// send OOB data, e.g. for closing.
		if v.writeClosed {
			return 0, errSocketWriteClosed
		}
		v.wrlock.Lock()

		thisBatch := min(toWrite, maxMsgSize)
		//fmt.Printf("WRITE: %d len=%x\n", int(v.fd), thisBatch)
		// Write message header
		binary.LittleEndian.PutUint32(b, uint32(thisBatch))
		n, err := v.write(b)
		if err != nil {
			fmt.Printf("Write 1\n")
			v.wrlock.Unlock()
			v.writeClosed = true
			return 0, err
		}
		if n != len(b) {
			fmt.Printf("Write 2\n")
			v.wrlock.Unlock()
			v.writeClosed = true
			return 0, errSocketMsgWrite
		}
		// Write data
		n, err = v.write(buf[written : written+thisBatch])
		if err != nil {
			fmt.Printf("Write 3\n")
			v.wrlock.Unlock()
			v.writeClosed = true
			return 0, err
		}
		if n != thisBatch {
			fmt.Printf("Write 4\n")
			v.wrlock.Unlock()
			v.writeClosed = true
			return 0, errSocketNotEnoughData
		}
		toWrite -= n
		written += n
		v.wrlock.Unlock()
	}

	return written, nil
}

// hvsockConn, SetDeadline(), SetReadDeadline(), and
// SetWriteDeadline() are OS specific.
