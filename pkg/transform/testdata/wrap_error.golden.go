package testpkg

import "fmt"

func handleErr(err error) error {
	return fmt.Errorf("failed: %w", err)
}

func handleErr2(err error) error {
	return fmt.Errorf("open: %w", err)
}
