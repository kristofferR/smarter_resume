package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "smarter_resume: %v\n", err)
		os.Exit(2)
	}

	code := run(context.Background(), cfg, os.Args[1:])
	os.Exit(code)
}
