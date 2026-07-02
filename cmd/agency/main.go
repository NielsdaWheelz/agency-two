package main

import (
	"os"

	"agency-two/internal/agency/app"
)

func main() {
	os.Exit(app.Run(os.Args[1:], os.Stdout, os.Stderr))
}
