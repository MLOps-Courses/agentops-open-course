package portalloc

import (
	"errors"
	"fmt"
	"net"
)

// Allocate reserves distinct loopback ports long enough to avoid duplicates within one request.
// The listeners close before return because callers need to hand the ports to child processes.
func Allocate(count int) ([]int, error) {
	if count < 1 || count > 64 {
		return nil, errors.New("port count must be between 1 and 64")
	}
	listeners := make([]net.Listener, 0, count)
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	ports := make([]int, 0, count)
	for range count {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("allocating loopback port: %w", err)
		}
		listeners = append(listeners, listener)
		address, ok := listener.Addr().(*net.TCPAddr)
		if !ok {
			return nil, fmt.Errorf("allocated listener has unexpected address %T", listener.Addr())
		}
		ports = append(ports, address.Port)
	}
	return ports, nil
}
