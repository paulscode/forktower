// Command forkbench builds a disposable two-chain test world on a local
// machine, so that split and breach scenarios can be rehearsed end to end.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	fmt.Fprintf(os.Stderr, "forkbench %s: not implemented yet\n", version)
	os.Exit(1)
}
