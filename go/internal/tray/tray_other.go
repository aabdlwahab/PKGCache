//go:build !linux && !windows && !darwin

package tray

import "context"

func run(_ context.Context, _ Options) error { return ErrUnsupported }
