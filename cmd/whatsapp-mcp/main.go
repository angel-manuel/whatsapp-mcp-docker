package main

import (
	"fmt"
	"os"
)

var version = "0.0.0-dev"

func main() {
	fmt.Fprintf(os.Stdout, "whatsapp-mcp %s\n", version)
}
