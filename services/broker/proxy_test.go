package broker

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/presmihaylov/shard/models"
	"github.com/presmihaylov/shard/pkg/proxy"
)

// SHARD-299: a guest that put its placeholder in Upgrade read the granted value back from the proxy's 502.
func TestAGuestNeverReadsASecretBackFromA502(t *testing.T) {
	records := fakeRecords{sandboxes: []models.Sandbox{{ID: "sb", Secrets: []string{"TOKEN"}, Address: netip.MustParsePrefix("127.0.0.1/8")}}}
	secrets := fakeSecrets{"TOKEN": {Name: "TOKEN", Placeholder: "mock-TOKEN", Destinations: []string{"api.example.com"}}}

	ca, err := proxy.LoadCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	server, err := proxy.New(proxy.Config{Address: netip.MustParseAddr("127.0.0.1"), CA: ca, Director: newBroker(t, records, secrets), Log: log.New(&logged, "", 0)})
	if err != nil {
		t.Fatal(err)
	}

	listeners := make([]net.Listener, 2)
	for i := range listeners {
		if listeners[i], err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listeners[0], listeners[1]) }()

	// A tab is a valid header byte but no protocol name, so the reverse proxy refuses the upgrade before it dials.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+listeners[0].Addr().String()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "api.example.com"
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "x\tmock-TOKEN")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	// The proxy logs after it answers, so the log is read once the proxy has stopped.
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}

	if resp.StatusCode != http.StatusBadGateway || strings.Contains(string(body), "real-TOKEN") {
		t.Errorf("the guest got %d %s, want a 502 without the value of TOKEN", resp.StatusCode, body)
	}
	if strings.Contains(logged.String(), "real-TOKEN") {
		t.Errorf("the proxy log holds the value of TOKEN:\n%s", logged.String())
	}
}
