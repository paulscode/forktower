// Command forktowerd is the Forktower daemon.
package main

import (
	"fmt"
	"os"
)

// version is set at build time via -ldflags. "dev" when built without it.
var version = "dev"

func main() {
	fmt.Fprintf(os.Stderr, "forktowerd %s: not implemented yet\n", version)
	os.Exit(1)
}
