package workbench

import "testing"

func TestGuestOrcaCLIReducesArgumentsToScopedOperations(t *testing.T) {
	watch := externalSessionWatch{Guest: true, Connection: "guest-independent", RuntimeID: "instance-a", Workspace: "/projects/quant"}
	env := []string{"--environment", watch.Connection, "--json"}
	for _, tc := range []struct {
		args []string
		op   string
	}{
		{[]string{"agent", "list", "--worktree", "path:/projects/quant"}, "agents"},
		{[]string{"terminal", "list", "--worktree", "path:/projects/quant", "--limit", "200"}, "terminals"},
		{[]string{"terminal", "wait", "--terminal", "term_fixture", "--for", "exit", "--timeout-ms", "30000"}, "wait"},
		{[]string{"worktree", "show", "--worktree", "id:fixture::/projects/quant"}, "worktree"},
		{[]string{"terminal", "create", "--worktree", "id:fixture", "--command", "sh -c whoami", "--keep-open"}, ""},
		{[]string{"agent", "list", "--worktree", "path:/projects/another"}, ""},
		{[]string{"open"}, ""},
	} {
		op, ok := parseGuestOrcaCLI(watch, append(tc.args, env...))
		if (tc.op != "") != ok || (ok && (op.Operation != tc.op || op.Workspace != watch.Workspace ||
			op.Runtime != "independent" || op.RuntimeID != watch.RuntimeID)) {
			t.Errorf("%q produced %+v, accepted=%t", tc.args, op, ok)
		}
	}
}
