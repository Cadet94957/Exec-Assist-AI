package google

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestClientIsReadOnly fails if this package gains any way to change a
// contractor's calendar.
//
// The OAuth scope in google.go is a promise made to Google. This is the
// guarantee: even if that scope were widened by accident, or a token arrived
// carrying more permission than we asked for, there would be nothing here to
// call with it. A customer's calendar is theirs, and the cheapest way to never
// damage one is to hold no code that could.
//
// OAuth token revocation is intentionally implemented in this package too, but
// it changes the authorization grant, not a calendar resource. It is therefore
// excluded from this calendar-client guard below. If a future change adds a
// calendar write, this test should fail and that change should be deliberate.
//
// It reads the package's own syntax tree rather than grepping, so prose in a
// comment — the scope comment uses the word "delete" — cannot fail the build,
// and a write verb cannot hide inside one either.
func TestClientIsReadOnly(t *testing.T) {
	// The HTTP methods that can change state, as both the net/http constant
	// name and the bare string someone might pass to http.NewRequest. GET and
	// HEAD are deliberately absent: they are the only two the Calendar API
	// client may use.
	banned := map[string]string{
		"MethodPost":   "http.MethodPost",
		"MethodPut":    "http.MethodPut",
		"MethodPatch":  "http.MethodPatch",
		"MethodDelete": "http.MethodDelete",
	}
	bannedLiteral := map[string]bool{
		"POST": true, "PUT": true, "PATCH": true, "DELETE": true,
	}

	const guidance = "\nThis package must only read the calendar. If a future phase genuinely needs to write to a " +
		"calendar, that is a deliberate decision with its own OAuth scope, its own consent " +
		"screen and its own review — so change this test on purpose, not by accident."

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	fset := token.NewFileSet()
	checked := 0

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// revoke.go talks to Google's OAuth revocation endpoint, not the
		// Calendar API. It is intentionally allowed to POST there so disconnect
		// can revoke the refresh token as well as deleting the local credential.
		if name == "revoke.go" {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++

		ast.Inspect(file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.SelectorExpr:
				if want, bad := banned[v.Sel.Name]; bad {
					t.Errorf("%s: %s appears in the Google client.%s",
						fset.Position(v.Pos()), want, guidance)
				}
			case *ast.BasicLit:
				if v.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(v.Value)
				if err == nil && bannedLiteral[strings.ToUpper(s)] {
					t.Errorf("%s: the string %q appears in the Google client.%s",
						fset.Position(v.Pos()), s, guidance)
				}
			}
			return true
		})
	}

	// A test that silently checks nothing is worse than no test, because it
	// reads as passing. If the files are ever renamed or moved, fail loudly.
	if checked == 0 {
		t.Fatal("no non-test Go files found in this package; this test is not checking anything")
	}
}

// TestScopeIsReadOnly pins the scope itself, so widening it has to be a
// deliberate edit to a test rather than a one-word change nobody reviews.
func TestScopeIsReadOnly(t *testing.T) {
	const want = "https://www.googleapis.com/auth/calendar.events.readonly"
	if Scope != want {
		t.Errorf("Scope = %q, want %q\nThis product reads a contractor's calendar; it does not change it.", Scope, want)
	}
}
