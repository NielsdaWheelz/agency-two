package main

import (
	"os"

	"agency-two/internal/agency/app"
)

func main() {
	os.Exit(app.RunSupervisor(os.Args[1:], os.Stdout, os.Stderr))
}
