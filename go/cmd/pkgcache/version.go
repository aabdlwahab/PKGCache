package main

import (
	"context"
	"fmt"

	"github.com/brightskies/pkgreg/internal/buildinfo"
)

func runVersion(_ context.Context, _ []string) error {
	fmt.Println(buildinfo.Get().String())
	return nil
}
