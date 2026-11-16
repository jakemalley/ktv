package main

import (
	"fmt"
	"os"

	"github.com/jakemalley/ktv/internal/ktv"
)

func main() {
	if err := ktv.Run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "ktv: %v\n", err)
		os.Exit(1)
	}
}
