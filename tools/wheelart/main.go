// Command wheelart writes the wheel's banner images to assets/wheel.
//
//	go run ./tools/wheelart
//
// Run it after changing the wedge table or internal/wheelart, and commit the result:
// TestCommittedAssetsMatchTheGenerator fails until the files on disk match the code.
package main

import (
	"fmt"
	"image/png"
	"os"
	"path/filepath"

	"github.com/6586x57890143/peregrine/internal/wheelart"
)

func main() {
	dir := filepath.Join("assets", "wheel")
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for name, img := range wheelart.All() {
		f, err := os.Create(filepath.Join(dir, name))
		if err == nil {
			err = png.Encode(f, img)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, name, err)
			os.Exit(1)
		}
	}
	fmt.Println("wrote", len(wheelart.All()), "images to", dir)
}
