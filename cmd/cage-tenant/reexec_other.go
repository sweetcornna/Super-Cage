//go:build !unix

package main

import "errors"

func reexecSelf(string, []string, []string) error {
	return errors.New("in-place self-restart requires a Unix host")
}
