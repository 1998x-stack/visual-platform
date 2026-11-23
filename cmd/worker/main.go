// Worker execution will be implemented in M2/M3. This entrypoint deliberately
// refuses to start rather than falsely advertising readiness.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "worker not implemented: M2/M3 upload pipeline is required")
	os.Exit(78)
}
