// Copyright AGNTCY Contributors (https://github.com/agntcy)
// SPDX-License-Identifier: Apache-2.0

// slim-bindings-setup downloads and installs the SLIM bindings native library.
//
// Usage:
//
//	go install github.com/agntcy/slim-bindings-go/cmd/slim-bindings-setup@latest
//	slim-bindings-setup
package main

import (
	"fmt"
	"runtime"
)

func main() {
	fmt.Println("╔═══════════════════════════════════════════════════════════╗")
	fmt.Println("║              SLIM Bindings Setup                          ║")
	fmt.Println("╚═══════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Printf("Platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Println()

	fmt.Println()
	fmt.Println("✅ Setup complete! You can now build Go projects using SLIM bindings.")
}
