package firecracker_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// The test binary plays firecracker when the driver execs it with this set; a second one makes it die at once.
const (
	fakeEnv    = "FIRECRACKER_FAKE"
	fakeDieEnv = "FIRECRACKER_FAKE_DIE"
)

// echoPort is the one guest port the fake listens on; a connect to any other is refused the way firecracker does it.
const echoPort = 5000

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) == "1" {
		if err := fakeVMM(); err != nil {
			fmt.Fprintln(os.Stderr, "fake firecracker:", err)
			os.Exit(1)
		}

		return
	}
	os.Setenv(fakeEnv, "1")
	os.Exit(m.Run())
}

// seen is what the fake was told, written beside its socket after every call so a test can read the order and the payloads.
type seen struct {
	Calls   []string          `json:"calls"`
	Machine json.RawMessage   `json:"machine"`
	Boot    json.RawMessage   `json:"boot"`
	Drives  []json.RawMessage `json:"drives"`
	Network json.RawMessage   `json:"network"`
	Vsock   json.RawMessage   `json:"vsock"`
	State   string            `json:"state"`
}

// fakeVMM is firecracker without KVM: the API on --api-sock, the vsock proxy on the uds_path, and a console line on stdout.
func fakeVMM() error {
	flags := flag.NewFlagSet("fake-firecracker", flag.ContinueOnError)
	socket := flags.String("api-sock", "", "")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if os.Getenv(fakeDieEnv) != "" {
		fmt.Fprintln(os.Stderr, os.Getenv(fakeDieEnv))

		return errors.New("told to die")
	}
	fmt.Println("fake firecracker console")

	listener, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	f := &fake{socket: *socket, seen: seen{State: "Not started"}}
	if err := f.persist(); err != nil {
		return err
	}
	server := &http.Server{Handler: f} //nolint:gosec // a fake behind a unix socket needs no timeouts
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	<-signals

	return errors.Join(listener.Close(), server.Close(), <-served)
}

type fake struct {
	socket string
	mu     sync.Mutex
	seen   seen
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}
	f.seen.Calls = append(f.seen.Calls, r.Method+" "+r.URL.Path)
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"fake","state":%q,"vmm_version":"0.0.0","app_name":"Firecracker"}`, f.seen.State)

		return
	}
	if f.seen.State != "Not started" {
		fault(w, http.StatusBadRequest, "The requested operation is not supported after starting the microVM.")

		return
	}
	refusal, err := f.apply(r.URL.Path, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}
	if refusal != "" {
		fault(w, http.StatusBadRequest, refusal)

		return
	}
	if err := f.persist(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apply takes one PUT the way firecracker would; a refusal is in its own words, an error is the fake's own.
func (f *fake) apply(path string, body []byte) (string, error) {
	switch {
	case path == "/machine-config":
		var m struct {
			VCPUs int64 `json:"vcpu_count"`
		}
		if err := json.Unmarshal(body, &m); err != nil {
			return "", err
		}
		if m.VCPUs < 1 || m.VCPUs > 32 {
			return "The vCPU number is invalid!", nil
		}
		f.seen.Machine = body
	case path == "/boot-source":
		f.seen.Boot = body
	case strings.HasPrefix(path, "/drives/"):
		f.seen.Drives = append(f.seen.Drives, body)
	case strings.HasPrefix(path, "/network-interfaces/"):
		f.seen.Network = body
	case path == "/vsock":
		f.seen.Vsock = body
	case path == "/actions":
		if f.seen.Boot == nil {
			return "Cannot start microvm without kernel configuration.", nil
		}
		if err := f.listenVsock(); err != nil {
			return "", err
		}
		f.seen.State = "Running"
	default:
		return "no route for " + path, nil
	}

	return "", nil
}

// listenVsock is the proxy firecracker opens at the uds_path on start: CONNECT <port> in, OK back, then the stream is the guest's.
func (f *fake) listenVsock() error {
	if f.seen.Vsock == nil {
		return nil
	}
	var v struct {
		Path string `json:"uds_path"`
	}
	if err := json.Unmarshal(f.seen.Vsock, &v); err != nil {
		return err
	}
	listener, err := net.Listen("unix", v.Path)
	if err != nil {
		return err
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go proxy(conn)
		}
	}()

	return nil
}

// proxy answers one host connection: the echo port accepts and echoes, any other port ends the connection without a word.
func proxy(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	port, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "CONNECT ")))
	if err != nil || port != echoPort {
		return
	}
	if _, err := fmt.Fprintf(conn, "OK %d\n", port); err != nil {
		return
	}
	_, _ = io.Copy(conn, reader)
}

func fault(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"fault_message": message})
}

func (f *fake) persist() error {
	encoded, err := json.Marshal(f.seen)
	if err != nil {
		return err
	}
	if err := os.WriteFile(f.socket+".seen.json", encoded, 0o600); err != nil {
		return err
	}

	return os.WriteFile(f.socket+".pid", []byte(strconv.Itoa(os.Getpid())), 0o600)
}
