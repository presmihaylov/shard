//go:build darwin && cgo

package vz

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"syscall"
	"time"

	"github.com/Code-Hex/vz/v3"
)

// VM wraps one framework VM. The device assembly follows hypeman's cmd/vz-shim/vm.go (8331138c), see NOTICE.
type VM struct {
	vm      *vz.VirtualMachine
	id      string
	netHost *os.File
	// The framework keeps the descriptors, not the files, so these stay referenced or a finalizer closes a live device.
	files []*os.File
}

// Close releases the files behind the devices, once the VM is gone.
func (m *VM) Close() error {
	var errs []error
	for _, f := range m.files {
		if err := f.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", f.Name(), err))
		}
	}

	return errors.Join(errs...)
}

// NewMachine builds the VM from cfg and validates it; nothing starts until Boot.
func NewMachine(cfg *Config) (*VM, error) {
	if err := CheckCPUs(cfg.CPUs, cpuRange()); err != nil {
		return nil, err
	}
	if err := CheckMemory(cfg.Memory, memoryRange()); err != nil {
		return nil, err
	}

	loader, err := vz.NewLinuxBootLoader(cfg.Kernel, vz.WithCommandLine(cfg.Cmdline), vz.WithInitrd(cfg.Initrd))
	if err != nil {
		return nil, fmt.Errorf("boot loader: %w", err)
	}
	vmc, err := vz.NewVirtualMachineConfiguration(loader, cpus(cfg.CPUs), Memory(cfg.Memory))
	if err != nil {
		return nil, fmt.Errorf("vm configuration: %w", err)
	}

	m := &VM{}
	if err := m.platform(vmc, cfg); err != nil {
		return nil, err
	}
	if err := m.console(vmc, cfg.Console); err != nil {
		return nil, err
	}
	if err := disk(vmc, cfg.Disk); err != nil {
		return nil, err
	}
	if cfg.Network {
		if err := m.network(vmc); err != nil {
			return nil, err
		}
	}

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("entropy device: %w", err)
	}
	vmc.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	vsock, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("vsock device: %w", err)
	}
	vmc.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vsock})

	if ok, err := vmc.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("vm configuration is invalid: %w", err)
	}
	if cfg.Restore != "" {
		if err := checkSaveRestore(vmc); err != nil {
			return nil, err
		}
	}

	m.vm, err = vz.NewVirtualMachine(vmc)
	if err != nil {
		return nil, fmt.Errorf("virtual machine: %w", err)
	}

	return m, nil
}

// Boot cold-starts the VM, or restores cfg.Restore over it and resumes.
func (m *VM) Boot(cfg *Config) error {
	if cfg.Restore == "" {
		if err := m.vm.Start(); err != nil {
			return fmt.Errorf("start the vm: %w", err)
		}

		return nil
	}

	if err := restore(m.vm, cfg.Restore); err != nil {
		return fmt.Errorf("restore %s: %w", cfg.Restore, err)
	}
	if err := m.vm.Resume(); err != nil {
		return fmt.Errorf("resume the restored vm: %w", err)
	}

	return nil
}

// Changed delivers every state change, so the shim can end when the VM does.
func (m *VM) Changed() <-chan vz.VirtualMachineState { return m.vm.StateChangedNotify() }

func (m *VM) State() State {
	return states[m.vm.State()]
}

func (m *VM) MachineID() string { return m.id }

func (m *VM) Pause() error {
	if err := m.vm.Pause(); err != nil {
		return fmt.Errorf("pause the vm: %w", err)
	}

	return nil
}

func (m *VM) Resume() error {
	if err := m.vm.Resume(); err != nil {
		return fmt.Errorf("resume the vm: %w", err)
	}

	return nil
}

func (m *VM) Save(path string) error {
	if err := save(m.vm, path); err != nil {
		return fmt.Errorf("save the vm state to %s: %w", path, err)
	}

	return nil
}

func (m *VM) Stop() error {
	if err := m.vm.Stop(); err != nil {
		return fmt.Errorf("stop the vm: %w", err)
	}

	return nil
}

// A guest that listens answers a connect at once; one that does not never calls back, since the framework "does nothing" for it.
const connectTimeout = 5 * time.Second

func (m *VM) Connect(port uint32) (net.Conn, error) {
	type result struct {
		conn *vz.VirtioSocketConnection
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := m.vm.SocketDevices()[0].Connect(port)
		done <- result{conn, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			return nil, fmt.Errorf("connect to guest vsock port %d: %w", port, r.err)
		}

		return r.conn, nil
	case <-time.After(connectTimeout):
		// A callback that fires late finds nobody, so the connection it carries is closed here rather than leaked.
		go func() {
			r := <-done
			if r.conn == nil {
				return
			}
			if err := r.conn.Close(); err != nil {
				log.Printf("vsock port %d: close a connection that answered late: %v", port, err)
			}
		}()

		return nil, fmt.Errorf("connect to guest vsock port %d: nothing listens within %s", port, connectTimeout)
	}
}

// Network is the host end of the frames socketpair; nil when the VM was built without a network.
func (m *VM) Network() (*os.File, error) {
	if m.netHost == nil {
		return nil, errors.New("the vm has no network device")
	}

	return m.netHost, nil
}

var states = map[vz.VirtualMachineState]State{
	vz.VirtualMachineStateStopped:   StateStopped,
	vz.VirtualMachineStateRunning:   StateRunning,
	vz.VirtualMachineStatePaused:    StatePaused,
	vz.VirtualMachineStateError:     StateError,
	vz.VirtualMachineStateStarting:  StateStarting,
	vz.VirtualMachineStatePausing:   StatePausing,
	vz.VirtualMachineStateResuming:  StateResuming,
	vz.VirtualMachineStateStopping:  StateStopping,
	vz.VirtualMachineStateSaving:    StateSaving,
	vz.VirtualMachineStateRestoring: StateRestoring,
}

func cpuRange() Range {
	return Range{Min: uint64(vz.VirtualMachineConfigurationMinimumAllowedCPUCount()), Max: uint64(vz.VirtualMachineConfigurationMaximumAllowedCPUCount())}
}

func memoryRange() Range {
	return Range{Min: vz.VirtualMachineConfigurationMinimumAllowedMemorySize(), Max: vz.VirtualMachineConfigurationMaximumAllowedMemorySize()}
}

func cpus(n uint) uint {
	if n == 0 {
		return DefaultCPUs(runtime.NumCPU(), cpuRange())
	}

	return n
}

// A restore refuses an identifier other than the saved one, so the caller persists what is made here.
func (m *VM) platform(vmc *vz.VirtualMachineConfiguration, cfg *Config) error {
	var id *vz.GenericMachineIdentifier
	var err error
	if cfg.MachineID == "" {
		id, err = vz.NewGenericMachineIdentifier()
	}
	if cfg.MachineID != "" {
		var raw []byte
		raw, err = base64.StdEncoding.DecodeString(cfg.MachineID)
		if err != nil {
			return fmt.Errorf("decode the machine identifier: %w", err)
		}
		id, err = vz.NewGenericMachineIdentifierWithData(raw)
	}
	if err != nil {
		return fmt.Errorf("machine identifier: %w", err)
	}

	platform, err := vz.NewGenericPlatformConfiguration(vz.WithGenericMachineIdentifier(id))
	if err != nil {
		return fmt.Errorf("platform configuration: %w", err)
	}
	vmc.SetPlatformVirtualMachineConfiguration(platform)
	m.id = base64.StdEncoding.EncodeToString(id.DataRepresentation())

	return nil
}

// The console is write-only into a log file; the guest's stdin is the vsock exec stream, never the serial line.
func (m *VM) console(vmc *vz.VirtualMachineConfiguration, path string) error {
	in, err := os.Open(os.DevNull)
	if err != nil {
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open the console log: %w", err)
	}
	m.files = append(m.files, in, out)

	attachment, err := vz.NewFileHandleSerialPortAttachment(in, out)
	if err != nil {
		return fmt.Errorf("console attachment: %w", err)
	}
	port, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(attachment)
	if err != nil {
		return fmt.Errorf("console device: %w", err)
	}
	vmc.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{port})

	return nil
}

func disk(vmc *vz.VirtualMachineConfiguration, path string) error {
	if path == "" {
		return nil
	}

	attachment, err := vz.NewDiskImageStorageDeviceAttachment(path, false)
	if err != nil {
		return fmt.Errorf("disk attachment for %s: %w", path, err)
	}
	block, err := vz.NewVirtioBlockDeviceConfiguration(attachment)
	if err != nil {
		return fmt.Errorf("block device: %w", err)
	}
	vmc.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{block})

	return nil
}

// One frame per datagram is what the file-handle device wants; the daemon takes the host end by the network verb.
func (m *VM) network(vmc *vz.VirtualMachineConfiguration) error {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return fmt.Errorf("network socketpair: %w", err)
	}
	guest := os.NewFile(uintptr(fds[0]), "vmnet-guest")
	m.netHost = os.NewFile(uintptr(fds[1]), "vmnet-host")
	m.files = append(m.files, guest, m.netHost)

	attachment, err := vz.NewFileHandleNetworkDeviceAttachment(guest)
	if err != nil {
		return fmt.Errorf("network attachment: %w", err)
	}
	device, err := vz.NewVirtioNetworkDeviceConfiguration(attachment)
	if err != nil {
		return fmt.Errorf("network device: %w", err)
	}
	mac, err := vz.NewMACAddress(net.HardwareAddr{0x02, 0x73, 0x68, 0x61, 0x72, 0x64})
	if err != nil {
		return fmt.Errorf("mac address: %w", err)
	}
	device.SetMACAddress(mac)
	vmc.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{device})

	return nil
}
