package main

import (
	"os"

	"github.com/tcurtsinger/GlideMind/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
