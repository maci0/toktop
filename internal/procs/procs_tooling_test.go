package procs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Both OS-tooling listers here run on a dashboard timer, and each spawns a
// wrapper that can outlive its own deadline. The fix for that is core.GroupKill
// plus the core.KillGroup every caller defers beside it, and every other
// exec.CommandContext caller in the tree already carries both.
//
// The pair cannot be observed from a test on the platform that lacks them:
// core.GroupKill is a documented no-op on windows, so there is nothing on the
// built command to assert against, and the windows lister cannot run on the
// host that would run this test at all. That is exactly how listWindows came
// to ship without either call while its darwin sibling had both, so this guard
// reads the two sources and walks the lister's own body for the calls. It is
// structural rather than a substring search on purpose: reformatting the file
// cannot pass or fail it, and a call reached through any spelling of its name
// is the thing being asserted.

var osToolingListers = []struct{ file, lister string }{
	{"procs_darwin.go", "listDarwin"},
	{"procs_windows.go", "listWindows"},
}

// GroupKill is the call that has to survive: KillGroup without it is a no-op
// on both platforms, there being no group of the command's own to address.
// Asserting the pair rather than the one keeps a future edit from adding the
// useless half for its own sake.
func TestEveryProcessListerArmsTheGroupKill(t *testing.T) {
	for _, tc := range osToolingListers {
		t.Run(tc.lister, func(t *testing.T) {
			if !listerBodyMentions(t, tc.file, tc.lister, func(n ast.Node) bool {
				return callsNamed(n, "GroupKill")
			}) {
				t.Errorf("%s does not call core.GroupKill: a wrapper that outlives its own deadline leaves a "+
					"survivor per sweep, and exec never signals the group on the WaitDelay path", tc.lister)
			}
			if !listerBodyMentions(t, tc.file, tc.lister, func(n ast.Node) bool {
				return callsNamed(n, "KillGroup")
			}) {
				t.Errorf("%s does not defer core.KillGroup: a wrapper that exits on its own with a child of "+
					"its own still holding the pipe is released by WaitDelay with that child unsignalled", tc.lister)
			}
		})
	}
}

// The group kill above is only reachable if the pipe grace is armed first:
// without WaitDelay, Output waits on the tool's pipe for as long as the
// grandchild holding it lives, so the kill is never reached in the first place.
func TestEveryProcessListerArmsThePipeGrace(t *testing.T) {
	for _, tc := range osToolingListers {
		t.Run(tc.lister, func(t *testing.T) {
			if !listerBodyMentions(t, tc.file, tc.lister, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok {
					return false
				}
				for _, lhs := range as.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "WaitDelay" {
						return true
					}
				}
				return false
			}) {
				t.Errorf("%s never sets cmd.WaitDelay, so Output waits on the tool's pipe until the grandchild "+
					"holding it exits, and the group kill is never reached", tc.lister)
			}
		})
	}
}

// listerBodyMentions parses file and reports whether match holds for some node
// in fn's own body. A fn the file does not define fails the test rather than
// reading as a silent pass: a renamed lister would otherwise turn this whole
// guard into a test that asserts nothing.
func listerBodyMentions(t *testing.T, file, fn string, match func(ast.Node) bool) bool {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	for _, decl := range parsed.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn || fd.Body == nil {
			continue
		}
		found := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if match(n) {
				found = true
			}
			return true
		})
		return found
	}
	t.Fatalf("%s defines no %s to inspect: this guard no longer names the function it was written for", file, fn)
	return false
}

// callsNamed reports whether n is a call to name, matched on the last element
// of the callee's path so the qualifier (core.GroupKill) is left to the caller.
func callsNamed(n ast.Node, name string) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name == name
	case *ast.Ident:
		return fun.Name == name
	}
	return false
}
