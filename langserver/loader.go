package langserver

import (
	"context"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"runtime/pprof"
	"strings"
	"time"

	opentracing "github.com/opentracing/opentracing-go"
	"github.com/sourcegraph/go-langserver/gotype"

	"github.com/sourcegraph/go-langserver/pkg/lsp"
	"github.com/sourcegraph/jsonrpc2"

	"golang.org/x/tools/go/loader"
)

// Cancel any ongoing operations, create a new context, return it.
func (h *LangHandler) updateContext() context.Context {
	h.adamfMutex.Lock()
	if h.cancelOngoingOperations != nil {
		h.cancelOngoingOperations()
	}
	realCtx, cancel := context.WithCancel(context.Background())
	h.cancelOngoingOperations = cancel
	h.adamfMutex.Unlock()
	return realCtx
}

// TODO(adamf): Split this into several functions. Diagnostics should be
// happening asynchronously and should not have a context.
// Typecheck the document referred to by fileURI. Send diagnostics as appropriate.
func (h *LangHandler) adamfDiagnostics(ctx context.Context, conn jsonrpc2.JSONRPC2, fileURI lsp.DocumentURI) {
	if !isFileURI(fileURI) {
		log.Println("Invalid File URI:", fileURI)
	}
	origFilename := h.FilePath(fileURI)

	profileFile, err := os.Create("/tmp/profile.pprof")
	start := time.Now()
	if err != nil {
		log.Println("error making profile", err)
		return
	}
	/*err = pprof.StartCPUProfile(profileFile)
	if err != nil {
		log.Println("could not start cpu profile", err)
	} else {
	*/
	defer func() {
		log.Println("Total time", time.Since(start))
		err := pprof.WriteHeapProfile(profileFile)
		if err != nil {
			log.Println("error writing heap profile", err)
		}
	}()
	//}

	realCtx := h.updateContext()
	bctx := h.BuildContext(realCtx)
	// cgo is not supported.
	bctx.CgoEnabled = false
	errs := gotype.CheckFile(origFilename, bctx, realCtx)

	diags := make(diagnostics)

	// Make sure that origFilename is represented to cover the case where the
	// final error was just fixed in this file.
	//
	// TODO(adamf): This doesn't really cover all cases where there was
	// previously an error. For example, we can fix other packages to now
	// successfully compile. It would be better to integrate this with the
	// caching/state tracking mechanism.
	diags[origFilename] = nil

	for _, err := range errs {
		var p token.Position
		var msg string
		switch e := err.(type) {
		case types.Error:
			p = e.Fset.Position(e.Pos)
			msg = e.Msg
		case *scanner.Error:
			p = e.Pos
			msg = e.Msg
		default:
			log.Printf("Unknown error type %T", err)
			return
		}
		diag := &lsp.Diagnostic{
			Range: lsp.Range{
				Start: lsp.Position{
					Line:      p.Line - 1,
					Character: p.Column - 1,
				},
				// TODO: Fix
				End: lsp.Position{
					Line:      p.Line,
					Character: p.Column,
				},
			},
			Severity: lsp.Error,
			Source:   "go",
			Message:  strings.TrimSpace(msg),
		}
		diags[p.Filename] = append(diags[p.Filename], diag)
	}

	// Do not send diagnostics if our context has since expired.
	if realCtx.Err() != nil {
		log.Println("Context expired")
		return
	}

	if err := h.publishAdamfDiagnostics(realCtx, conn, diags); err != nil {
		log.Printf("warning: failed to send diagnostics: %s.", err)
	}
}

func (h *LangHandler) typecheck(ctx context.Context, conn jsonrpc2.JSONRPC2, fileURI lsp.DocumentURI, position lsp.Position) (*token.FileSet, *ast.Ident, []ast.Node, *loader.Program, *loader.PackageInfo, *token.Pos, error) {
	parentSpan := opentracing.SpanFromContext(ctx)
	span := parentSpan.Tracer().StartSpan("langserver-go: load program",
		opentracing.Tags{"fileURI": fileURI},
		opentracing.ChildOf(parentSpan.Context()),
	)
	ctx = opentracing.ContextWithSpan(ctx, span)
	defer span.Finish()

	if !isFileURI(fileURI) {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("typechecking of out-of-workspace URI (%q) is not yet supported", fileURI)
	}

	filename := h.FilePath(fileURI)

	contents, err := h.readFile(ctx, fileURI)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	offset, valid, why := offsetForPosition(contents, position)
	if !valid {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("invalid position: %s:%d:%d (%s)", filename, position.Line, position.Character, why)
	}

	bctx := h.BuildContext(ctx)

	bpkg, err := ContainingPackage(bctx, filename)
	if mpErr, ok := err.(*build.MultiplePackageError); ok {
		bpkg, err = buildPackageForNamedFileInMultiPackageDir(bpkg, mpErr, filepath.Base(filename))
		if err != nil {
			return nil, nil, nil, nil, nil, nil, err
		}
	} else if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}

	// TODO(sqs): do all pkgs in workspace together?
	fset, prog, diags, err := h.cachedTypecheck(ctx, bctx, bpkg)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}

	if len(diags) > 0 {
		go func() {
			if err := h.publishDiagnostics(ctx, conn, diags); err != nil {
				log.Printf("warning: failed to send diagnostics: %s.", err)
			}
		}()
	}

	start := posForFileOffset(fset, filename, offset)
	if start == token.NoPos {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("invalid location: %s:#%d", filename, offset)
	}

	pkg, nodes, _ := prog.PathEnclosingInterval(start, start)
	if len(nodes) == 0 {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("no node found at %s offset %d", fset.Position(start), offset)
	}
	node, ok := nodes[0].(*ast.Ident)
	if !ok {
		lineCol := func(p token.Pos) string {
			pp := fset.Position(p)
			return fmt.Sprintf("%d:%d", pp.Line, pp.Column)
		}
		return fset, nil, nodes, prog, pkg, &start, &invalidNodeError{
			Node: nodes[0],
			msg:  fmt.Sprintf("invalid node: %s (%s-%s)", reflect.TypeOf(nodes[0]).Elem(), lineCol(nodes[0].Pos()), lineCol(nodes[0].End())),
		}
	}
	return fset, node, nodes, prog, pkg, &start, nil
}

type invalidNodeError struct {
	Node ast.Node
	msg  string
}

func (e *invalidNodeError) Error() string {
	return e.msg
}

func posForFileOffset(fset *token.FileSet, filename string, offset int) token.Pos {
	var f *token.File
	fset.Iterate(func(ff *token.File) bool {
		if ff.Name() == filename {
			f = ff
			return false // break out of loop
		}
		return true
	})
	if f == nil {
		return token.NoPos
	}
	return f.Pos(offset)
}

// buildPackageForNamedFileInMultiPackageDir returns a package that
// refer to the package named by filename. If there are multiple
// (e.g.) main packages in a dir in separate files, this lets you
// synthesize a *build.Package that just refers to one. It's necessary
// to handle that case.
func buildPackageForNamedFileInMultiPackageDir(bpkg *build.Package, m *build.MultiplePackageError, filename string) (*build.Package, error) {
	copy := *bpkg
	bpkg = &copy

	// First, find which package name each filename is in.
	fileToPkgName := make(map[string]string, len(m.Files))
	for i, f := range m.Files {
		fileToPkgName[f] = m.Packages[i]
	}

	pkgName := fileToPkgName[filename]
	if pkgName == "" {
		return nil, fmt.Errorf("package %q in %s has no file %q", bpkg.ImportPath, bpkg.Dir, filename)
	}

	filterToFilesInPackage := func(files []string, pkgName string) []string {
		var keep []string
		for _, f := range files {
			if fileToPkgName[f] == pkgName {
				keep = append(keep, f)
			}
		}
		return keep
	}

	// Trim the *GoFiles fields to only those files in the same
	// package.
	bpkg.Name = pkgName
	if pkgName == "main" {
		// TODO(sqs): If the package name is "main", and there are
		// multiple main packages that are separate programs (and,
		// e.g., expected to be run directly run `go run main1.go
		// main2.go`), then this will break because it will try to
		// compile them all together. There's no good way to handle
		// that case that I can think of, other than with heuristics.
	}
	var nonXTestPkgName, xtestPkgName string
	if strings.HasSuffix(pkgName, "_test") {
		nonXTestPkgName = strings.TrimSuffix(pkgName, "_test")
		xtestPkgName = pkgName
	} else {
		nonXTestPkgName = pkgName
		xtestPkgName = pkgName + "_test"
	}
	bpkg.GoFiles = filterToFilesInPackage(bpkg.GoFiles, nonXTestPkgName)
	bpkg.TestGoFiles = filterToFilesInPackage(bpkg.TestGoFiles, nonXTestPkgName)
	bpkg.XTestGoFiles = filterToFilesInPackage(bpkg.XTestGoFiles, xtestPkgName)

	return bpkg, nil
}

type typecheckKey struct {
	importPath, srcDir, name string

	// TODO(sqs): needs to include a list of files in the key...there
	// can be multiple packages (e.g., build-tag-disabled main.go
	// files) with the same names

	// TODO(sqs): include build context in key
}

type typecheckResult struct {
	fset *token.FileSet
	prog *loader.Program
	err  error
}

func (h *LangHandler) cachedTypecheck(ctx context.Context, bctx *build.Context, bpkg *build.Package) (*token.FileSet, *loader.Program, diagnostics, error) {
	parentSpan := opentracing.SpanFromContext(ctx)
	span := parentSpan.Tracer().StartSpan("langserver-go: typecheck",
		opentracing.Tags{"pkg": bpkg.ImportPath},
		opentracing.ChildOf(parentSpan.Context()),
	)
	ctx = opentracing.ContextWithSpan(ctx, span)
	defer span.Finish()

	var diags diagnostics
	r := h.typecheckCache.Get(typecheckKey{bpkg.ImportPath, bpkg.Dir, bpkg.Name}, func() interface{} {
		res := &typecheckResult{
			fset: token.NewFileSet(),
		}
		res.prog, diags, res.err = typecheck(ctx, res.fset, bctx, bpkg, h.getFindPackageFunc())
		return res
	})
	if r == nil {
		// This can happen if we panic
		return nil, nil, diags, nil
	}
	res := r.(*typecheckResult)
	return res.fset, res.prog, diags, res.err
}

// TODO(sqs): allow typechecking just a specific file not in a package, too
func typecheck(ctx context.Context, fset *token.FileSet, bctx *build.Context, bpkg *build.Package, findPackage FindPackageFunc) (*loader.Program, diagnostics, error) {
	var typeErrs []error
	conf := loader.Config{
		Fset: fset,
		TypeChecker: types.Config{
			DisableUnusedImportCheck: true,
			FakeImportC:              true,
			Error: func(err error) {
				typeErrs = append(typeErrs, err)
			},
		},
		Build:       bctx,
		Cwd:         bpkg.Dir,
		AllowErrors: true,
		TypeCheckFuncBodies: func(p string) bool {
			return bpkg.ImportPath == p
		},
		ParserMode: parser.AllErrors | parser.ParseComments, // prevent parser from bailing out
		FindPackage: func(bctx *build.Context, importPath, fromDir string, mode build.ImportMode) (*build.Package, error) {
			// When importing a package, ignore any
			// MultipleGoErrors. This occurs, e.g., when you have a
			// main.go with "// +build ignore" that imports the
			// non-main package in the same dir.
			bpkg, err := findPackage(ctx, bctx, importPath, fromDir, mode)
			if err != nil && !isMultiplePackageError(err) {
				return bpkg, err
			}
			return bpkg, nil
		},
	}

	// Hover needs this info, otherwise we could zero out the unnecessary
	// results to save memory.
	//
	// TODO(sqs): investigate other ways to speed this up using
	// AfterTypeCheck; see
	// https://sourcegraph.com/github.com/golang/tools@5ffc3249d341c947aa65178abbf2253ed49c9e03/-/blob/cmd/guru/referrers.go#L148.
	//
	// 	conf.AfterTypeCheck = func(info *loader.PackageInfo, files []*ast.File) {
	// 		if !conf.TypeCheckFuncBodies(info.Pkg.Path()) {
	// 			clearInfoFields(info)
	// 		}
	// 	}
	//

	var goFiles []string
	goFiles = append(goFiles, bpkg.GoFiles...)
	goFiles = append(goFiles, bpkg.TestGoFiles...)
	if strings.HasSuffix(bpkg.Name, "_test") {
		goFiles = append(goFiles, bpkg.XTestGoFiles...)
	}
	for i, filename := range goFiles {
		goFiles[i] = filepath.Join(bpkg.Dir, filename)
	}
	conf.CreateFromFilenames(bpkg.ImportPath, goFiles...)
	prog, err := conf.Load()
	if err != nil && prog == nil {
		return nil, nil, err
	}
	diags, err := errsToDiagnostics(typeErrs, prog)
	if err != nil {
		return nil, nil, err
	}
	return prog, diags, nil
}

func clearInfoFields(info *loader.PackageInfo) {
	// TODO(adonovan): opt: save memory by eliminating unneeded scopes/objects.
	// (Requires go/types change for Go 1.7.)
	//   info.Pkg.Scope().ClearChildren()

	// Discard the file ASTs and their accumulated type
	// information to save memory.
	info.Files = nil
	info.Defs = make(map[*ast.Ident]types.Object)
	info.Uses = make(map[*ast.Ident]types.Object)
	info.Implicits = make(map[ast.Node]types.Object)

	// Also, disable future collection of wholly unneeded
	// type information for the package in case there is
	// more type-checking to do (augmentation).
	info.Types = nil
	info.Scopes = nil
	info.Selections = nil
}

func isMultiplePackageError(err error) bool {
	_, ok := err.(*build.MultiplePackageError)
	return ok
}
