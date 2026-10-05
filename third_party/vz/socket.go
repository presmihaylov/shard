package vz

/*
#cgo darwin CFLAGS: -mmacosx-version-min=11 -x objective-c -fno-objc-arc
#cgo darwin LDFLAGS: -lobjc -framework Foundation -framework Virtualization
# include "virtualization_11.h"
*/
import "C"
import (
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"runtime/cgo"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/Code-Hex/vz/v3/internal/objc"
)

// SocketDeviceConfiguration for a socket device configuration.
type SocketDeviceConfiguration interface {
	objc.NSObject

	socketDeviceConfiguration()
}

type baseSocketDeviceConfiguration struct{}

func (*baseSocketDeviceConfiguration) socketDeviceConfiguration() {}

var _ SocketDeviceConfiguration = (*VirtioSocketDeviceConfiguration)(nil)

// VirtioSocketDeviceConfiguration is a configuration of the Virtio socket device.
//
// This configuration creates a Virtio socket device for the guest which communicates with the host through the Virtio interface.
// Only one Virtio socket device can be used per virtual machine.
// see: https://developer.apple.com/documentation/virtualization/vzvirtiosocketdeviceconfiguration?language=objc
type VirtioSocketDeviceConfiguration struct {
	*pointer

	*baseSocketDeviceConfiguration
}

// NewVirtioSocketDeviceConfiguration creates a new VirtioSocketDeviceConfiguration.
//
// This is only supported on macOS 11 and newer, error will
// be returned on older versions.
func NewVirtioSocketDeviceConfiguration() (*VirtioSocketDeviceConfiguration, error) {
	if err := macOSAvailable(11); err != nil {
		return nil, err
	}

	config := newVirtioSocketDeviceConfiguration(C.newVZVirtioSocketDeviceConfiguration())

	objc.SetFinalizer(config, func(self *VirtioSocketDeviceConfiguration) {
		objc.Release(self)
	})
	return config, nil
}

func newVirtioSocketDeviceConfiguration(ptr unsafe.Pointer) *VirtioSocketDeviceConfiguration {
	return &VirtioSocketDeviceConfiguration{
		pointer: objc.NewPointer(ptr),
	}
}

// VirtioSocketDevice a device that manages port-based connections between the guest system and the host computer.
//
// Don’t create a VirtioSocketDevice struct directly. Instead, when you request a socket device in your configuration,
// the virtual machine creates it and you can get it via SocketDevices method.
// see: https://developer.apple.com/documentation/virtualization/vzvirtiosocketdevice?language=objc
type VirtioSocketDevice struct {
	dispatchQueue unsafe.Pointer
	*pointer
}

func newVirtioSocketDevice(ptr, dispatchQueue unsafe.Pointer) *VirtioSocketDevice {
	return &VirtioSocketDevice{
		dispatchQueue: dispatchQueue,
		pointer:       objc.NewPointer(ptr),
	}
}

// Listen creates a new VirtioSocketListener which is a struct that listens for port-based connection requests
// from the guest operating system.
//
// Be sure to close the listener by calling `VirtioSocketListener.Close` after used this one.
//
// This is only supported on macOS 11 and newer, error will
// be returned on older versions.
func (v *VirtioSocketDevice) Listen(port uint32) (*VirtioSocketListener, error) {
	if err := macOSAvailable(11); err != nil {
		return nil, err
	}

	ch := make(chan connResults, 1) // should I increase more caps?

	handle := cgo.NewHandle(func(conn *VirtioSocketConnection, err error) {
		ch <- connResults{conn, err}
	})
	ptr := C.newVZVirtioSocketListener(
		C.uintptr_t(handle),
	)
	listener := &VirtioSocketListener{
		pointer:     objc.NewPointer(ptr),
		vsockDevice: v,
		port:        port,
		handle:      handle,
		acceptch:    ch,
	}

	C.VZVirtioSocketDevice_setSocketListenerForPort(
		objc.Ptr(v),
		v.dispatchQueue,
		objc.Ptr(listener),
		C.uint32_t(port),
	)

	return listener, nil
}

// pendingConnects holds one live dial per connectToPort call, keyed by a monotonic id the C layer passes back; a never-reused id lets a late callback find its slot gone instead of resolving a freed cgo handle (SHARD-619).
var (
	pendingConnects   sync.Map
	pendingConnectSeq atomic.Uint64
)

//export connectionHandler
func connectionHandler(connPtr, errPtr unsafe.Pointer, idUintptr C.uintptr_t) {
	v, ok := pendingConnects.LoadAndDelete(uint64(idUintptr))
	if !ok {
		// The dial was cancelled and its slot already reclaimed, so drop the late connection rather than deliver to a gone caller (SHARD-619).
		dropConn(connPtr, errPtr)

		return
	}
	switch handler := v.(type) {
	case *managedConnect:
		fn, dead := handler.take()
		if dead {
			dropConn(connPtr, errPtr)

			return
		}
		deliver(fn, connPtr, errPtr)
	case func(*VirtioSocketConnection, error):
		deliver(handler, connPtr, errPtr)
	}
}

// deliver turns the framework's raw pointers into a conn or an error and hands them to fn.
func deliver(fn func(*VirtioSocketConnection, error), connPtr, errPtr unsafe.Pointer) {
	if err := newNSError(errPtr); err != nil {
		fn(nil, err)

		return
	}
	conn, err := newVirtioSocketConnection(connPtr)
	fn(conn, err)
}

// dropConn closes a connection that completed after the caller cancelled the dial, so a cancelled connect leaks neither the handle nor the conn (SHARD-619).
func dropConn(connPtr, errPtr unsafe.Pointer) {
	if err := newNSError(errPtr); err != nil {
		return
	}
	conn, err := newVirtioSocketConnection(connPtr)
	if err != nil {
		// Pres ruled 2026-10-05: log and return; a cancelled dial has no caller to take the error, and a conn we could not build has nothing to close.
		log.Printf("vsock: build a connection that completed after the dial was cancelled: %v", err)

		return
	}
	if err := conn.Close(); err != nil {
		log.Printf("vsock: close a connection that completed after the dial was cancelled: %v", err)
	}
}

// Connect Initiates a connection to the specified port of the guest operating system.
//
// This method initiates the connection asynchronously, and executes the completion handler when the results are available.
// If the guest operating system doesn’t listen for connections to the specified port, this method does nothing.
//
// For a successful connection, this method sets the sourcePort property of the resulting VZVirtioSocketConnection object to a random port number.
// see: https://developer.apple.com/documentation/virtualization/vzvirtiosocketdevice/3656677-connecttoport?language=objc
func (v *VirtioSocketDevice) Connect(port uint32) (*VirtioSocketConnection, error) {
	ch := make(chan connResults, 1)
	id := pendingConnectSeq.Add(1)
	pendingConnects.Store(id, func(conn *VirtioSocketConnection, err error) {
		ch <- connResults{conn, err}
		close(ch)
	})
	C.VZVirtioSocketDevice_connectToPort(
		objc.Ptr(v),
		v.dispatchQueue,
		C.uint32_t(port),
		C.uintptr_t(id),
	)
	result := <-ch
	runtime.KeepAlive(v)
	return result.conn, result.err
}

// managedConnect carries a connect's callback, its registry id and whether the caller cancelled the dial (SHARD-619).
type managedConnect struct {
	id   uint64
	mu   sync.Mutex
	fn   func(*VirtioSocketConnection, error)
	dead bool
}

// cancel reclaims the registry slot so a guest that never answers no longer leaks it, and marks the dial dead so a callback that still wins the slot delivers to nobody (SHARD-619).
func (m *managedConnect) cancel() {
	pendingConnects.Delete(m.id)
	m.mu.Lock()
	m.dead = true
	m.fn = nil
	m.mu.Unlock()
}

// take reads the callback and the dead flag together, so the callback and cancel never race over fn (SHARD-619).
func (m *managedConnect) take() (func(*VirtioSocketConnection, error), bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.fn, m.dead
}

// ConnectHandler starts a connect and runs fn once from the framework's callback, on the VM queue; the returned cancel reclaims the dial's registry slot so a timed-out dial neither leaks nor delivers (SHARD-619).
func (v *VirtioSocketDevice) ConnectHandler(port uint32, fn func(*VirtioSocketConnection, error)) (cancel func()) {
	id := pendingConnectSeq.Add(1)
	managed := &managedConnect{id: id, fn: fn}
	pendingConnects.Store(id, managed)
	C.VZVirtioSocketDevice_connectToPort(
		objc.Ptr(v),
		v.dispatchQueue,
		C.uint32_t(port),
		C.uintptr_t(id),
	)
	runtime.KeepAlive(v)

	return managed.cancel
}

type connResults struct {
	conn *VirtioSocketConnection
	err  error
}

// VirtioSocketListener a struct that listens for port-based connection requests from the guest operating system.
//
// see: https://developer.apple.com/documentation/virtualization/vzvirtiosocketlistener?language=objc
type VirtioSocketListener struct {
	*pointer
	vsockDevice *VirtioSocketDevice
	handle      cgo.Handle
	port        uint32
	acceptch    chan connResults
	closeOnce   sync.Once
}

var _ net.Listener = (*VirtioSocketListener)(nil)

// Accept implements the Accept method in the Listener interface; it waits for the next call and returns a net.Conn.
func (v *VirtioSocketListener) Accept() (net.Conn, error) {
	return v.AcceptVirtioSocketConnection()
}

// AcceptVirtioSocketConnection accepts the next incoming call and returns the new connection.
func (v *VirtioSocketListener) AcceptVirtioSocketConnection() (*VirtioSocketConnection, error) {
	result := <-v.acceptch
	return result.conn, result.err
}

// Close stops listening on the virtio socket.
func (v *VirtioSocketListener) Close() error {
	v.closeOnce.Do(func() {
		C.VZVirtioSocketDevice_removeSocketListenerForPort(
			objc.Ptr(v.vsockDevice),
			v.vsockDevice.dispatchQueue,
			C.uint32_t(v.port),
		)
		v.handle.Delete()
	})
	return nil
}

// Addr returns the listener's network address, a *VirtioSocketListenerAddr.
func (v *VirtioSocketListener) Addr() net.Addr {
	const VMADDR_CID_HOST = 2 // copied from unix pacage
	return &VirtioSocketListenerAddr{
		CID:  VMADDR_CID_HOST,
		Port: v.port,
	}
}

// VirtioSocketListenerAddr represents a network end point address for the vsock protocol.
type VirtioSocketListenerAddr struct {
	CID  uint32
	Port uint32
}

var _ net.Addr = (*VirtioSocketListenerAddr)(nil)

// Network returns "vsock".
func (a *VirtioSocketListenerAddr) Network() string { return "vsock" }

// String returns string of "<cid>:<port>"
func (a *VirtioSocketListenerAddr) String() string { return fmt.Sprintf("%d:%d", a.CID, a.Port) }

//export shouldAcceptNewConnectionHandler
func shouldAcceptNewConnectionHandler(cgoHandleUintptr C.uintptr_t, connPtr, devicePtr unsafe.Pointer) C.bool {
	cgoHandle := cgo.Handle(cgoHandleUintptr)
	handler := cgoHandle.Value().(func(*VirtioSocketConnection, error))

	// see: startHandler
	conn, err := newVirtioSocketConnection(connPtr)
	go handler(conn, err)
	return (C.bool)(true)
}

// VirtioSocketConnection is a port-based connection between the guest operating system and the host computer.
//
// You don’t create connection objects directly. When the guest operating system initiates a connection, the virtual machine creates
// the connection object and passes it to the appropriate VirtioSocketListener struct, which forwards the object to its delegate.
//
// This is implemented net.Conn interface. This is generated from duplicated a file descriptor which is returned
// from virtualization.framework. macOS cannot connect directly to the Guest operating system using vsock. The　vsock
// connection must always be made via virtualization.framework. The diagram looks like this.
//
// ┌─────────┐                     ┌────────────────────────────┐               ┌────────────┐
// │  macOS  │<─── unix socket ───>│  virtualization.framework  │<─── vsock ───>│  Guest OS  │
// └─────────┘                     └────────────────────────────┘               └────────────┘
//
// You will notice that this is not vsock in using this library. However, all data this connection goes through to the vsock
// connection to which the Guest OS is connected.
//
// This struct does not have any pointers for objects of the Objective-C. Because the various values
// of the VZVirtioSocketConnection object handled by Objective-C are no longer needed after the conversion
// to the Go struct.
//
// see: https://developer.apple.com/documentation/virtualization/vzvirtiosocketconnection?language=objc
type VirtioSocketConnection struct {
	rawConn         net.Conn
	destinationPort uint32
	sourcePort      uint32
}

var _ net.Conn = (*VirtioSocketConnection)(nil)

func newVirtioSocketConnection(ptr unsafe.Pointer) (*VirtioSocketConnection, error) {
	vzVirtioSocketConnection := C.convertVZVirtioSocketConnection2Flat(ptr)
	// The VZVirtioSocketConnection owns its fd and closes it when it is destroyed, so only a dup is ours to close.
	syscall.ForkLock.RLock()
	fd, err := syscall.Dup(int(vzVirtioSocketConnection.fileDescriptor))
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "")
	defer file.Close()
	rawConn, err := net.FileConn(file)
	if err != nil {
		return nil, err
	}
	conn := &VirtioSocketConnection{
		rawConn:         rawConn,
		destinationPort: (uint32)(vzVirtioSocketConnection.destinationPort),
		sourcePort:      (uint32)(vzVirtioSocketConnection.sourcePort),
	}
	return conn, nil
}

// Read reads data from connection of the vsock protocol.
func (v *VirtioSocketConnection) Read(b []byte) (n int, err error) { return v.rawConn.Read(b) }

// Write writes data to the connection of the vsock protocol.
func (v *VirtioSocketConnection) Write(b []byte) (n int, err error) { return v.rawConn.Write(b) }

// Close will be called when caused something error in socket.
func (v *VirtioSocketConnection) Close() error {
	return v.rawConn.Close()
}

// LocalAddr returns the local network address.
func (v *VirtioSocketConnection) LocalAddr() net.Addr { return v.rawConn.LocalAddr() }

// RemoteAddr returns the remote network address.
func (v *VirtioSocketConnection) RemoteAddr() net.Addr { return v.rawConn.RemoteAddr() }

// SetDeadline sets the read and write deadlines associated
// with the connection. It is equivalent to calling both
// SetReadDeadline and SetWriteDeadline.
func (v *VirtioSocketConnection) SetDeadline(t time.Time) error { return v.rawConn.SetDeadline(t) }

// SetReadDeadline sets the deadline for future Read calls
// and any currently-blocked Read call.
// A zero value for t means Read will not time out.
func (v *VirtioSocketConnection) SetReadDeadline(t time.Time) error {
	return v.rawConn.SetReadDeadline(t)
}

// SetWriteDeadline sets the deadline for future Write calls
// and any currently-blocked Write call.
// Even if write times out, it may return n > 0, indicating that
// some of the data was successfully written.
// A zero value for t means Write will not time out.
func (v *VirtioSocketConnection) SetWriteDeadline(t time.Time) error {
	return v.rawConn.SetWriteDeadline(t)
}

// DestinationPort returns the destination port number of the connection.
func (v *VirtioSocketConnection) DestinationPort() uint32 {
	return v.destinationPort
}

// SourcePort returns the source port number of the connection.
func (v *VirtioSocketConnection) SourcePort() uint32 {
	return v.sourcePort
}
