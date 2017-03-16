// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

// TODO: use x/tools/go/ssa?

// uses interface{} instead of ast.Node for node slices
func (r *reducer) reduceNode(v interface{}) bool {
	if r.didChange {
		return false
	}
	switch x := v.(type) {
	case *ast.ImportSpec:
		return false
	case *[]ast.Stmt:
		r.removeStmt(x)
		r.inlineBlock(x)
	case *ast.IfStmt:
		undo := r.afterDelete(x.Init, x.Cond, x.Else)
		if r.changeStmt(x.Body) {
			r.logChange(x, "if a { b } -> { b }")
			break
		}
		undo()
		if x.Else != nil {
			undo := r.afterDelete(x.Init, x.Cond, x.Body)
			if r.changeStmt(x.Else) {
				r.logChange(x, "if a {...} else c -> c")
				break
			}
			undo()
		}
	case *ast.Ident:
		obj := r.info.Uses[x]
		if obj == nil {
			break
		}
		bt, ok := obj.Type().(*types.Basic)
		if !ok {
			break
		}
		if bt.Info()&types.IsUntyped == 0 {
			break
		}
		var expr ast.Expr
		ast.Inspect(r.file, func(node ast.Node) bool {
			ds, ok := node.(*ast.DeclStmt)
			if !ok {
				return true
			}
			gd := ds.Decl.(*ast.GenDecl)
			if gd.Tok != token.CONST {
				return false
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if r.info.Defs[name] == obj {
						expr = vs.Values[i]
						return false
					}
				}
			}
			return true
		})
		if expr != nil && r.changeExpr(expr) {
			r.logChange(x, "const inlined")
		}
	case *ast.BasicLit:
		r.reduceLit(x)
	case *ast.SliceExpr:
		r.reduceSlice(x)
	case *ast.CompositeLit:
		if len(x.Elts) == 0 {
			break
		}
		orig := x.Elts
		undo := r.afterDeleteExprs(x.Elts)
		if x.Elts = nil; r.okChange() {
			t := "T"
			switch x.Type.(type) {
			case *ast.ArrayType:
				t = "[]" + t
			}
			r.logChange(x, "%s{a, b} -> %s{}", t, t)
			break
		}
		undo()
		x.Elts = orig
	case *ast.BinaryExpr:
		undo := r.afterDelete(x.Y)
		if r.changeExpr(x.X) {
			r.logChange(x, "a %v b -> a", x.Op)
			break
		}
		undo()
		undo = r.afterDelete(x.X)
		if r.changeExpr(x.Y) {
			r.logChange(x, "a %v b -> b", x.Op)
			break
		}
		undo()
	case *ast.ParenExpr:
		if r.changeExpr(x.X) {
			r.logChange(x, "(a) -> a")
		}
	case *ast.IndexExpr:
		undo := r.afterDelete(x.Index)
		if r.changeExpr(x.X) {
			r.logChange(x, "a[b] -> a")
			break
		}
		undo()
	case *ast.StarExpr:
		if r.changeExpr(x.X) {
			r.logChange(x, "*a -> a")
		}
	case *ast.UnaryExpr:
		if r.changeExpr(x.X) {
			r.logChange(x, "%va -> a", x.Op)
		}
	case *ast.GoStmt:
		if r.changeStmt(&ast.ExprStmt{X: x.Call}) {
			r.logChange(x, "go a() -> a()")
		}
	case *ast.DeferStmt:
		if r.changeStmt(&ast.ExprStmt{X: x.Call}) {
			r.logChange(x, "defer a() -> a()")
		}
	}
	return true
}

func (r *reducer) removeStmt(list *[]ast.Stmt) {
	orig := *list
	l := make([]ast.Stmt, len(orig)-1)
	for i, stmt := range orig {
		// discard those that will likely break compilation
		switch x := stmt.(type) {
		case *ast.DeclStmt:
			gd := x.Decl.(*ast.GenDecl)
			if !r.allUnusedNames(gd) {
				continue
			}
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE { // :=
				continue
			}
		}
		undo := r.afterDelete(stmt)
		copy(l, orig[:i])
		copy(l[i:], orig[i+1:])
		*list = l
		if r.okChange() {
			r.mergeLinesNode(stmt)
			r.logChange(stmt, "%s removed", nodeType(stmt))
			return
		}
		undo()
	}
	*list = orig
}

// allUnusedNames reports whether all delcs in a GenDecl are vars or
// consts with empty (underscore) or unused names.
func (r *reducer) allUnusedNames(gd *ast.GenDecl) bool {
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			return false
		}
		for _, name := range vs.Names {
			if len(r.useIdents[r.info.Defs[name]]) > 0 {
				return false
			}
		}
	}
	return true
}

func nodeType(n ast.Node) string {
	s := fmt.Sprintf("%T", n)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

func (r *reducer) mergeLinesNode(node ast.Node) {
	r.mergeLines(node.Pos(), node.End()+1)
}

func (r *reducer) mergeLines(start, end token.Pos) {
	file := r.fset.File(start)
	l1 := file.Line(start)
	l2 := file.Line(end)
	for l1 < l2 {
		file.MergeLine(l1)
		l1++
	}
}

func (r *reducer) inlineBlock(list *[]ast.Stmt) {
	orig := *list
	type undoIdent struct {
		id   *ast.Ident
		name string
	}
	var undoIdents []undoIdent
	for i, stmt := range orig {
		bl, ok := stmt.(*ast.BlockStmt)
		if !ok {
			continue
		}
		// TODO: handle more complex name collisions,
		// e.g. doesCollide, doesNot := ...
		for _, stmt := range bl.List {
			switch x := stmt.(type) {
			case *ast.AssignStmt:
				if x.Tok == token.ASSIGN || len(x.Lhs) != 1 {
					break
				}
				id, ok := x.Lhs[0].(*ast.Ident)
				if !ok {
					break
				}
				obj := r.info.Defs[id]
				scope := obj.Parent()
				if scope.Parent().Lookup(id.Name) == nil {
					break
				}
				origName := id.Name
				newName := id.Name + "_"
				for scope.Lookup(newName) != nil {
					newName += "_"
				}
				id.Name = newName
				for _, id := range r.useIdents[obj] {
					undoIdents = append(undoIdents, undoIdent{
						id:   id,
						name: origName,
					})
					id.Name = newName
				}
			}
		}
		var l []ast.Stmt
		l = append(l, orig[:i]...)
		l = append(l, bl.List...)
		l = append(l, orig[i+1:]...)
		*list = l
		if r.okChange() {
			r.mergeLines(bl.Pos(), bl.List[0].Pos())
			r.mergeLines(bl.List[len(bl.List)-1].End(), bl.End())
			r.logChange(stmt, "block inlined")
			return
		}
	}
	for _, ui := range undoIdents {
		ui.id.Name = ui.name
	}
	*list = orig
}

func (r *reducer) afterDeleteExprs(exprs []ast.Expr) (undo func()) {
	nodes := make([]ast.Node, len(exprs))
	for i, expr := range exprs {
		nodes[i] = expr
	}
	return r.afterDelete(nodes...)
}

func (r *reducer) afterDelete(nodes ...ast.Node) (undo func()) {
	type redoImp struct {
		name, path string
	}
	var imps []redoImp
	type redoVar struct {
		id   *ast.Ident
		name string
		asgn *ast.AssignStmt
	}
	var vars []redoVar

	for _, obj := range r.unusedAfterDelete(nodes...) {
		switch x := obj.(type) {
		case *types.PkgName:
			name := x.Name()
			if x.Imported().Name() == name {
				// import wasn't named
				name = ""
			}
			path := x.Imported().Path()
			astutil.DeleteNamedImport(r.fset, r.file, name, path)
			imps = append(imps, redoImp{name, path})
		case *types.Var:
			ast.Inspect(r.file, func(node ast.Node) bool {
				switch x := node.(type) {
				case *ast.Ident:
					vars = append(vars, redoVar{x, x.Name, nil})
					if r.info.Defs[x] == obj {
						x.Name = "_"
					}
				case *ast.AssignStmt:
					// TODO: support more complex assigns
					if len(x.Lhs) != 1 {
						return false
					}
					id, ok := x.Lhs[0].(*ast.Ident)
					if !ok {
						return false
					}
					if r.info.Defs[id] != obj {
						return false
					}
					vars = append(vars, redoVar{id, id.Name, x})
					id.Name, x.Tok = "_", token.ASSIGN
					return false
				}
				return true
			})
		}
	}
	return func() {
		for _, imp := range imps {
			astutil.AddNamedImport(r.fset, r.file, imp.name, imp.path)
		}
		for _, rvar := range vars {
			rvar.id.Name = rvar.name
			if rvar.asgn != nil {
				rvar.asgn.Tok = token.DEFINE
			}
		}
	}
}

func (r *reducer) unusedAfterDelete(nodes ...ast.Node) (objs []types.Object) {
	remaining := make(map[types.Object]int)
	for _, node := range nodes {
		if node == nil {
			continue // for convenience
		}
		ast.Inspect(node, func(node ast.Node) bool {
			id, ok := node.(*ast.Ident)
			if !ok {
				return true
			}
			obj := r.info.Uses[id]
			if obj == nil {
				return false
			}
			if num, e := remaining[obj]; e {
				if num == 1 {
					objs = append(objs, obj)
				}
				remaining[obj]--
			} else if ids, e := r.useIdents[obj]; e {
				if len(ids) == 1 {
					objs = append(objs, obj)
				} else {
					remaining[obj] = len(ids) - 1
				}
			}
			return true
		})
	}
	return
}

func (r *reducer) changeStmt(stmt ast.Stmt) bool {
	orig := *r.stmt
	if *r.stmt = stmt; r.okChange() {
		return true
	}
	*r.stmt = orig
	return false
}

func (r *reducer) changeExpr(expr ast.Expr) bool {
	orig := *r.expr
	if *r.expr = expr; r.okChange() {
		return true
	}
	*r.expr = orig
	return false
}

func (r *reducer) reduceLit(l *ast.BasicLit) {
	orig := l.Value
	changeValue := func(val string) bool {
		if l.Value == val {
			return false
		}
		if l.Value = val; r.okChange() {
			return true
		}
		l.Value = orig
		return false
	}
	switch l.Kind {
	case token.STRING:
		if changeValue(`""`) {
			if len(orig) > 10 {
				orig = fmt.Sprintf(`%s..."`, orig[:7])
			}
			r.logChange(l, `%s -> ""`, orig)
		}
	case token.INT:
		if changeValue(`0`) {
			if len(orig) > 10 {
				orig = fmt.Sprintf(`%s...`, orig[:7])
			}
			r.logChange(l, `%s -> 0`, orig)
		}
	}
}

func (r *reducer) reduceSlice(sl *ast.SliceExpr) {
	if r.changeExpr(sl.X) {
		r.logChange(sl, "a[b:] -> a")
		return
	}
	for i, expr := range [...]*ast.Expr{
		&sl.Max,
		&sl.High,
		&sl.Low,
	} {
		orig := *expr
		if orig == nil {
			continue
		}
		if i == 0 {
			sl.Slice3 = false
		}
		if *expr = nil; r.okChange() {
			switch i {
			case 0:
				r.logChange(orig, "a[b:c:d] -> a[b:c]")
			case 1:
				r.logChange(orig, "a[b:c] -> a[b:]")
			case 2:
				r.logChange(orig, "a[b:c] -> a[:c]")
			}
			return
		}
		if i == 0 {
			sl.Slice3 = true
		}
		*expr = orig
	}
}
