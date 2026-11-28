package config

import (
	"fmt"
	"slices"
	"strings"
)

// builtinToolNamespaces are the namespaces of the Data and Action Gate's own
// tools and actions (memory.get, conclave.read, sablecourt.list_workloads,
// git.open_pr, sandbox.propose_promotion, ...). An upstream MCP server is
// exposed as "<name>.<tool>", so a server whose name is one of these could
// answer a call meant for a built-in tool.
var builtinToolNamespaces = []string{"memory", "conclave", "sablecourt", "git", "sandbox"}

// BuiltinToolNamespace reports whether a tool or server name falls in a
// built-in namespace and returns that namespace. It compares the part before
// the first dot, trimmed and in lower case, so "Memory" and "memory.v2" are
// refused too.
func BuiltinToolNamespace(name string) (string, bool) {
	head, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(name)), ".")
	if slices.Contains(builtinToolNamespaces, head) {
		return head, true
	}
	return "", false
}

// validateUpstreamServerNames refuses an mcpServers entry whose name is a
// built-in tool namespace.
func validateUpstreamServerNames(workload string, names []string) error {
	for _, n := range names {
		if ns, ok := BuiltinToolNamespace(n); ok {
			return fmt.Errorf("workload %q: mcpServers name %q is a built-in tool namespace (%s.*); pick another name", workload, n, ns)
		}
	}
	return nil
}
