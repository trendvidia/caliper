package testpkg

import "fmt"

func handleErr(err error) error {
	return fmt.Errorf("failed: %v", err)
}

func handleErr2(err error) error {
	return fmt.Errorf("open: %v", err)
}
