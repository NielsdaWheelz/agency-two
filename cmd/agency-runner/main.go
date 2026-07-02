package main

import (
	"os"

	"agency-two/internal/agency/runner"
)

func main() {
	os.Exit(runner.Run(os.Args[1:], os.Stdout, os.Stderr))
}
