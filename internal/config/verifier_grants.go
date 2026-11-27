package config

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

// verifierReadOnlyTools are the built-in Data Gate tools a verifier workload
// may hold. Each one only reads: memory.get and memory.search read Court
// Memory, conclave.read and conclave.relic_get read a room, and
// sablecourt.list_workloads lists the bundle's workloads. A tool that writes,
// deletes, posts or reaches an upstream server is left out, and so is any
// tool not named here.
var verifierReadOnlyTools = []string{
	"memory.get",
	"memory.search",
	"conclave.read",
	"conclave.relic_get",
	"sablecourt.list_workloads",
}

// verifierGrant says what a verifier workload may hold in one GrantSet field.
type verifierGrant int

const (
	// verifierGrantEmpty: the field must hold its zero value.
	verifierGrantEmpty verifierGrant = iota
	// verifierGrantReadOnlyTools: every entry must be in verifierReadOnlyTools.
	verifierGrantReadOnlyTools
)

// verifierGrantFields classifies every field of domain.GrantSet. A field
// missing here refuses any verifier workload, and a test fails until the
// field is classified.
var verifierGrantFields = map[string]verifierGrant{
	"ModelGate":  verifierGrantEmpty,
	"MCPTools":   verifierGrantReadOnlyTools,
	"MCPServers": verifierGrantEmpty,
	"Actions":    verifierGrantEmpty,
	"DelegateTo": verifierGrantEmpty,
	"Repos":      verifierGrantEmpty,
}

// validateVerifierGrants refuses a verifier workload that holds any grant
// outside the read-only allow-list (ADR-0010 amendment of 2026-11-27,
// invariant 4). It also refuses a read-only tool name that an upstream MCP
// server in the bundle would register under the same name. The loader already
// refuses an mcpServers name in a built-in namespace (tool_namespaces.go) and
// the gate never lets an upstream tool take a built-in name, so this is a
// third line.
func validateVerifierGrants(bundle *Bundle, name string, grants domain.GrantSet) error {
	v := reflect.ValueOf(grants)
	t := v.Type()
	for i := range t.NumField() {
		field := t.Field(i)
		label := grantLabel(field)
		class, ok := verifierGrantFields[field.Name]
		if !ok {
			return fmt.Errorf("verifier workload %q: grants.%s is not classified as read-only", name, label)
		}
		switch class {
		case verifierGrantEmpty:
			if !v.Field(i).IsZero() {
				return fmt.Errorf("verifier workload %q may hold only read-only grants, not grants.%s%s", name, label, verifierGrantNote(field.Name))
			}
		case verifierGrantReadOnlyTools:
			tools, _ := v.Field(i).Interface().([]string)
			for _, tool := range tools {
				if !slices.Contains(verifierReadOnlyTools, tool) {
					return fmt.Errorf("verifier workload %q: mcpTools entry %q is not read-only", name, tool)
				}
				if owner, ok := upstreamToolOwner(bundle, tool); ok {
					return fmt.Errorf("verifier workload %q: mcpTools entry %q is also an upstream tool of workload %q", name, tool, owner)
				}
			}
		default:
			return fmt.Errorf("verifier workload %q: grants.%s has no verifier rule", name, label)
		}
	}
	return nil
}

// verifierGrantNote explains a refusal where the reason is not obvious.
func verifierGrantNote(field string) string {
	if field == "ModelGate" {
		// The Model Gate lets a workload pick its model and fall back to
		// another provider per request, which the family check at dispatch
		// cannot see until the gate binds the chain's families to the lease.
		return "; the Model Gate does not check model families yet"
	}
	return ""
}

func grantLabel(f reflect.StructField) string {
	if tag, _, _ := strings.Cut(f.Tag.Get("json"), ","); tag != "" {
		return tag
	}
	return f.Name
}

// upstreamToolOwner returns a workload whose mcpServers register tool.
func upstreamToolOwner(bundle *Bundle, tool string) (string, bool) {
	names := make([]string, 0, len(bundle.Workloads))
	for n := range bundle.Workloads {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		for _, srv := range bundle.Workloads[n].Spec.Grants.MCPServers {
			for _, t := range srv.AllowedTools {
				if srv.Name+"."+strings.TrimSpace(t) == tool {
					return n, true
				}
			}
		}
	}
	return "", false
}
