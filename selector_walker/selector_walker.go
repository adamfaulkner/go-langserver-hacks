package selector_walker

import (
	"errors"
	"go/ast"
	"go/token"
)

type IdentFilter struct {
	All         bool
	Identifiers map[string]struct{}
}

func (i *IdentFilter) CheckIdent(ident string) bool {
	if i.All {
		return true
	}
	_, ok := i.Identifiers[ident]
	return ok
}

// A function declaration will match a identFilter if the the type is T or *T
// and T is in the set of identifiers.
//
// According to the go spec, the receiver type must be of the form T or *T
// where T is a type name.
func (i *IdentFilter) checkRecv(recv *ast.FieldList) bool {
	if i.All {
		return true
	}

	if recv.NumFields() != 1 {
		panic("Invalid receiver list, wrong length")
	}

	typeExpr := recv.List[0].Type
	var typeName string
	switch typeExprT := typeExpr.(type) {
	case *ast.Ident:
		typeName = typeExprT.Name
	case *ast.StarExpr:
		inner, ok := typeExprT.X.(*ast.Ident)
		if !ok {
			panic("Invalid recv, wrong type of type")
		}
		typeName = inner.Name
	default:
		panic("Invalid recv, wrong type of type")
	}

	_, ok := i.Identifiers[typeName]
	return ok
}

// FuncDecls match if they are a normal function and the name is in the
// identfilter, or if they're a method and the type is in the identfilter.
func (i *IdentFilter) CheckFuncDecl(fd *ast.FuncDecl) bool {
	if fd.Recv != nil {
		return i.checkRecv(fd.Recv)
	} else {
		return i.CheckIdent(fd.Name.String())
	}
}

// SelectorWalker offers a way to iterate over the selectors in top level
// declarations in a file. Note that in a top level declaration, the only valid
// selector is a qualified identifier (the go spec separates these from
// Selectors, but the ast package treats them the same way.)
type selectorWalker struct {
	// Contains the list of remaining decls to look at. These still need to be filtered with identFilter.
	declList []ast.Decl
	// When current decl is a GenDecl, this refers to the next spec to look at. These still need to be filtered with identFilter
	specList []ast.Spec
	// Contains the list of reminaing exprs to look at. These do not need to be filtered with identFilter by their nature.
	exprList []ast.Expr
	// Contains a filter to use for identifiers.
	idf IdentFilter
}

func NewSelectorWalker(f *ast.File, idf IdentFilter) *selectorWalker {
	return &selectorWalker{
		declList: f.Decls,
		idf:      idf,
	}

}

var SelectorWalkerFinished = errors.New("Finished walking")

func (s *selectorWalker) NextSelector() (ast.SelectorExpr, error) {
	if len(s.exprList) > 0 {
		return s.processExprList()
	}

	if len(s.specList) > 0 {
		return s.processSpecList()
	}

	if len(s.declList) > 0 {
		return s.processDeclList()
	}

	return ast.SelectorExpr{}, SelectorWalkerFinished
}

// Append types to exprList from a field list.
func (s *selectorWalker) appendFieldList(fl *ast.FieldList) {
	if len(fl.List) == 0 {
		return
	}

	for _, f := range fl.List {
		s.exprList = append(s.exprList, f.Type)
	}
}

func (s *selectorWalker) processExprList() (ast.SelectorExpr, error) {
	nextExpr := s.exprList[0]
	s.exprList = s.exprList[1:]

	switch neT := nextExpr.(type) {
	case *ast.SelectorExpr:
		// Finally! Base case.
		return *neT, nil

	case *ast.BadExpr:
		return ast.SelectorExpr{}, errors.New("BadExpr!")
	case *ast.Ident:
		// skip
	case *ast.Ellipsis:
		s.exprList = append(s.exprList, neT.Elt)
	case *ast.BasicLit:
		// skip
	case *ast.FuncLit:
		s.exprList = append(s.exprList, neT.Type)
	case *ast.CompositeLit:
		s.exprList = append(s.exprList, neT.Type)
		s.exprList = append(s.exprList, neT.Elts...)
	case *ast.ParenExpr:
		s.exprList = append(s.exprList, neT.X)
	case *ast.IndexExpr:
		s.exprList = append(s.exprList, neT.X)
		s.exprList = append(s.exprList, neT.Index)
	case *ast.SliceExpr:
		s.exprList = append(s.exprList, neT.X)
		s.exprList = append(s.exprList, neT.Low)
		s.exprList = append(s.exprList, neT.High)
		s.exprList = append(s.exprList, neT.Max)
	case *ast.TypeAssertExpr:
		s.exprList = append(s.exprList, neT.X)
		s.exprList = append(s.exprList, neT.Type)
	case *ast.CallExpr:
		s.exprList = append(s.exprList, neT.Fun)
		s.exprList = append(s.exprList, neT.Args...)
	case *ast.StarExpr:
		s.exprList = append(s.exprList, neT.X)
	case *ast.UnaryExpr:
		s.exprList = append(s.exprList, neT.X)
	case *ast.BinaryExpr:
		s.exprList = append(s.exprList, neT.X)
		s.exprList = append(s.exprList, neT.Y)
	case *ast.KeyValueExpr:
		s.exprList = append(s.exprList, neT.Key)
		s.exprList = append(s.exprList, neT.Value)
	case *ast.ArrayType:
		s.exprList = append(s.exprList, neT.Len) // Not necessary
		s.exprList = append(s.exprList, neT.Elt)
	case *ast.StructType:
		s.appendFieldList(neT.Fields)
	case *ast.FuncType:
		s.appendFieldList(neT.Params)
		if neT.Results != nil {
			s.appendFieldList(neT.Results)
		}
	case *ast.InterfaceType:
		s.appendFieldList(neT.Methods)
	case *ast.MapType:
		s.exprList = append(s.exprList, neT.Key)
		s.exprList = append(s.exprList, neT.Value)
	case *ast.ChanType:
		s.exprList = append(s.exprList, neT.Value)
	}
	return s.NextSelector()
}

func (s *selectorWalker) processSpecList() (ast.SelectorExpr, error) {
	nextSpec := s.specList[0]
	s.specList = s.specList[1:]

	switch nsT := nextSpec.(type) {
	case *ast.ValueSpec:
		for i, name := range nsT.Names {
			if s.idf.CheckIdent(name.Name) {
				s.exprList = append(s.exprList, nsT.Type)
				if len(nsT.Values) > i {
					s.exprList = append(s.exprList, nsT.Values[i])
				}
			}
		}

	case *ast.TypeSpec:
		if s.idf.CheckIdent(nsT.Name.Name) {
			s.exprList = append(s.exprList, nsT.Type)
		}

	default:
		return ast.SelectorExpr{}, errors.New("Unexpected spec.")

	}

	return s.NextSelector()
}

func (s *selectorWalker) processDeclList() (ast.SelectorExpr, error) {
	nextDecl := s.declList[0]
	s.declList = s.declList[1:]

	switch ndT := nextDecl.(type) {
	case *ast.BadDecl:
		return ast.SelectorExpr{}, errors.New("Bad Decl Found")
	case *ast.GenDecl:
		// We don't bother with imports.
		if ndT.Tok == token.IMPORT {
			return s.NextSelector()
		}
		s.specList = ndT.Specs
		return s.NextSelector()

	case *ast.FuncDecl:
		if s.idf.CheckFuncDecl(ndT) {
			s.exprList = []ast.Expr{ndT.Type}
		}
		return s.NextSelector()

	default:
		return ast.SelectorExpr{}, errors.New("Unexpected type of decl")
	}

}