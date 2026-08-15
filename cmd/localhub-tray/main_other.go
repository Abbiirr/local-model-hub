//go:build !windows

package main

import "fmt"

func main() {
	fmt.Println("localhub-tray is available on Windows only")
}
