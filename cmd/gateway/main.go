// Static Gateway implementation belongs to M4. It must not serve any artifact
// until OIDC and per-request authorization are integrated and tested.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "gateway not implemented: M4/M5 authz and content isolation are required")
	os.Exit(78)
}
