package storage_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestOnlyThisPackageReachesBbolt is the import invariant the whole layout rests on.
//
// internal/storage is the only package that knows a bucket exists. Everything above it gets a
// *storage.Reader or *storage.Writer, handed to a callback and bound to one transaction, and
// neither type has a method that starts a transaction. That is what makes the worst bug in the
// review UNWRITABLE rather than merely fixed: generation used to run inside a db.View and call
// helpers that each opened their own db.View, and bbolt holds mmaplock.RLock for a read
// transaction's whole life and takes the write lock to grow the mmap, so outer-read plus
// waiting-writer plus inner-read is a deadlock with no timeout and no recovery (SPEC.md section
// 8, finding 1).
//
// The import is the exact invariant to check. The bbolt API is unreachable without it, so if no
// file outside this package imports bbolt then no function outside this package can name a
// bucket, hold a handle, or start a transaction.
//
// # It scans the module now, and that is the M11c change
//
// The check used to live in internal/legacy and glob that package's files, because legacy was
// the only place the dependency could have come back. legacy is nearly gone and there are eight
// packages above the seam now, so the check moved here and widened: this is the package that
// owns the invariant, and the scan covers everything that is not it.
func TestOnlyThisPackageReachesBbolt(t *testing.T) {
	const bbolt = "go.etcd.io/bbolt"

	// This package's own directory, which is where the import belongs.
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}

	root := filepath.Join("..", "..")
	scanned := 0
	fset := token.NewFileSet()

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "voicenotes", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		abs, aerr := filepath.Abs(path)
		if aerr == nil && filepath.Dir(abs) == self {
			return nil
		}
		scanned++

		file, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly|parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		for _, imp := range file.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			if p == bbolt || strings.HasPrefix(p, bbolt+"/") {
				t.Errorf("%s imports %s. Only internal/storage may: everything above the seam "+
					"takes a *storage.Reader or *storage.Writer bound to one transaction, and "+
					"neither has a method that opens another, which is what makes the nested "+
					"transaction deadlock in finding 1 unwritable rather than merely fixed",
					path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// A walk that found nothing would pass silently, which is the failure mode this style of
	// test has: it would look like an invariant holding rather than like a glob that matched no
	// files. This is also why the check is a scan of every file rather than of one named file:
	// a structural check scoped to a single file stops being a check on the package the moment
	// the package grows a second one.
	if scanned < 20 {
		t.Fatalf("scanned only %d files outside this package; the walk is wrong and this test "+
			"proves nothing", scanned)
	}
}
