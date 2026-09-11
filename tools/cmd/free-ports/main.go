package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/MLOps-Courses/agentops-open-course/tools/internal/portalloc"
)

func main() {
	count := flag.Int("count", 1, "number of distinct loopback ports")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "free-ports does not accept positional arguments")
		os.Exit(2)
	}
	ports, err := portalloc.Allocate(*count)
	if err != nil {
		fmt.Fprintln(os.Stderr, "free-ports:", err)
		os.Exit(1)
	}
	for index, port := range ports {
		if index > 0 {
			fmt.Print(" ")
		}
		fmt.Print(port)
	}
	fmt.Println()
}
