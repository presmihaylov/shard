package firecracker

// Over is a client of a socket no Start or Adopt proved, for a test of what a call meets there.
func Over(socket, vsock string) *Client { return &Client{socket: socket, vsock: vsock} }
