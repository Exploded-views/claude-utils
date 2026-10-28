//go:build !unix

package main

import "errors"

func mkfifo(string) error { return errors.New("no FIFOs here") }

func syscallNonblock() int { return 0 }
