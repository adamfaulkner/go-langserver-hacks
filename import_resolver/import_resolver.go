package import_resolver

import (
	"go/ast"
	"go/build"
	"strings"
)

type findCacheKey struct {
	importDir  string
	importPath string
}

type importResolver struct {
	bctx *build.Context

	// pkgCache maps package source directory to complete build package. This
	// avoids repeated calls to bctx.ImportDir.
	pkgCache map[string]*build.Package

	// findCache maps import source directory & import path to package source
	// directory. This avoids repeated calls to bctx.Import with FindOnly mode.
	findCache map[findCacheKey]string
}

func NewImportResolver(bctx *build.Context) *importResolver {
	return &importResolver{
		bctx:      bctx,
		pkgCache:  map[string]*build.Package{},
		findCache: map[findCacheKey]string{},
	}
}

func trimLit(b *ast.BasicLit) string {
	return strings.Trim(b.Value, "\"")
}

// TODO: Probably have to handle test stuff here.

// Given a file, resolve returns the name -> package source dir mapping for all imports.
func (i *importResolver) Resolve(f *ast.File, sourceDir string) (map[string]string, error) {
	result := make(map[string]string, len(f.Imports))

	for _, imp := range f.Imports {
		if imp.Name != nil {
			// Super easy case - name maps to package.
			packageDir, err := i.getPackagePath(trimLit(imp.Path), sourceDir)
			if err != nil {
				return nil, err
			}

			result[imp.Name.String()] = packageDir
		} else {
			// No name, must load the package to get it.
			// We load package this janky way in order to populate the relevant
			// caches appropriately.
			packageDir, err := i.getPackagePath(trimLit(imp.Path), sourceDir)
			if err != nil {
				return nil, err
			}

			pkg, err := i.getPackage(packageDir)
			if err != nil {
				return nil, err
			}
			result[pkg.Name] = packageDir
		}
	}
	return result, nil
}

func (i *importResolver) getPackagePath(importPath string, srcDir string) (string, error) {
	fck := findCacheKey{
		importDir:  srcDir,
		importPath: importPath,
	}

	path, ok := i.findCache[fck]
	if ok {
		return path, nil
	}

	pkg, err := i.bctx.Import(importPath, srcDir, build.FindOnly)
	if err != nil {
		return "", err
	}

	i.findCache[fck] = pkg.Dir
	return pkg.Dir, nil
}

func (i *importResolver) getPackage(pkgDir string) (*build.Package, error) {
	pkg, ok := i.pkgCache[pkgDir]
	if ok {
		return pkg, nil
	}

	pkg, err := i.bctx.ImportDir(pkgDir, 0)
	if err != nil {
		return nil, err
	}
	i.pkgCache[pkgDir] = pkg
	return pkg, nil

}
