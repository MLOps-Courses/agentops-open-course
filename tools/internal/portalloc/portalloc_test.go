package portalloc

import "testing"

func TestAllocateReturnsDistinctUnprivilegedPorts(t *testing.T) {
	ports, err := Allocate(8)
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 8 {
		t.Fatalf("len(ports) = %d", len(ports))
	}
	seen := make(map[int]struct{}, len(ports))
	for _, port := range ports {
		if port < 1024 || port > 65535 {
			t.Fatalf("port = %d", port)
		}
		if _, exists := seen[port]; exists {
			t.Fatalf("duplicate port = %d", port)
		}
		seen[port] = struct{}{}
	}
}

func TestAllocateRejectsInvalidCount(t *testing.T) {
	for _, count := range []int{-1, 0, 65} {
		if _, err := Allocate(count); err == nil {
			t.Fatalf("Allocate(%d) error = nil", count)
		}
	}
}
