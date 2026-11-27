package config

import (
	"strings"
	"testing"
)

const hdr = "apiVersion: sablecourt.io/v1alpha1\n"

func workloadDoc(name, zone string) string {
	return hdr + `kind: Workload
metadata:
  name: ` + name + `
  zone: ` + zone + `
spec:
  class: deterministic-controller
  runtime:
    type: docker
    image: worker:0.1.0
`
}

func conclaveDoc(body string) string {
	return hdr + "kind: Conclave\nmetadata:\n  name: council\nspec:\n" + body
}

func TestLoadPathRefusesMalformedDocuments(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"bad header", "apiVersion: [\n", "parse type header"},
		{"wrong apiVersion", "apiVersion: sablecourt.io/v2\nkind: Workload\n", "unsupported apiVersion"},
		{"unknown kind", hdr + "kind: Secret\nmetadata: {name: x}\n", `unsupported kind "Secret"`},
		{"policy without name", hdr + "kind: Policy\nmetadata: {}\nspec: {defaultEffect: deny}\n", "Policy metadata.name is required"},
		{"duplicate policy", hdr + "kind: Policy\nmetadata: {name: p}\nspec: {defaultEffect: deny}\n---\n" + hdr + "kind: Policy\nmetadata: {name: p}\nspec: {defaultEffect: deny}\n", `duplicate Policy "p"`},
		{"policy unknown field", hdr + "kind: Policy\nmetadata: {name: p}\nspec: {defaultEffect: deny, widen: true}\n", "decode Policy"},
		{"budget without name", hdr + "kind: Budget\nmetadata: {}\nspec: {daily: {maxCostUSD: 1}}\n", "Budget metadata.name is required"},
		{"duplicate budget", hdr + "kind: Budget\nmetadata: {name: b}\nspec: {daily: {maxCostUSD: 1}}\n---\n" + hdr + "kind: Budget\nmetadata: {name: b}\nspec: {daily: {maxCostUSD: 1}}\n", `duplicate Budget "b"`},
		{"budget unknown field", hdr + "kind: Budget\nmetadata: {name: b}\nspec: {unlimited: true}\n", "decode Budget"},
		{"negative budget", hdr + "kind: Budget\nmetadata: {name: b}\nspec: {daily: {maxCostUSD: -1}}\n", "limits must be non-negative"},
		{"workload without name", hdr + "kind: Workload\nmetadata: {zone: runtime}\nspec: {class: deterministic-controller}\n", "Workload metadata.name is required"},
		{"duplicate workload", workloadDoc("w", "runtime") + "---\n" + workloadDoc("w", "runtime"), `duplicate Workload "w"`},
		{"workflow without name", hdr + "kind: Workflow\nmetadata: {}\nspec: {}\n", "Workflow metadata.name is required"},
		{"workflow unknown field", hdr + "kind: Workflow\nmetadata: {name: f}\nspec: {shell: true}\n", "decode Workflow"},
		{"schedule without name", hdr + "kind: Schedule\nmetadata: {}\nspec: {}\n", "Schedule metadata.name is required"},
		{"schedule unknown field", hdr + "kind: Schedule\nmetadata: {name: s}\nspec: {root: true}\n", "decode Schedule"},
		{"conclave without name", hdr + "kind: Conclave\nmetadata: {}\nspec: {}\n", "Conclave metadata.name is required"},
		{"conclave unknown field", conclaveDoc("  admins: [x]\n"), "decode Conclave"},
		{"duplicate conclave", workloadDoc("w", "runtime") + "---\n" + conclaveDoc("  vassals: [{role: r, workload: w}]\n") + "---\n" + conclaveDoc("  vassals: [{role: r, workload: w}]\n"), `duplicate Conclave "council"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadPath(writeTempManifest(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LoadPath = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadPathConclaveValidation(t *testing.T) {
	workers := workloadDoc("worker", "runtime") + "---\n" + workloadDoc("sandboxed", "sandbox") + "---\n"
	cases := []struct {
		name    string
		spec    string
		wantErr string // empty: valid
	}{
		{"default activation", "  vassals: [{role: reader, workload: worker}]\n", ""},
		{"no vassals", "  vassals: []\n", "at least one vassal"},
		{"unknown budget", "  budgetRef: missing\n  vassals: [{role: r, workload: worker}]\n", `referenced budget "missing" not found`},
		{"negative messages", "  wards: {maxMessages: -1}\n  vassals: [{role: r, workload: worker}]\n", "non-negative"},
		{"negative ping pong", "  wards: {maxPingPong: -1}\n  vassals: [{role: r, workload: worker}]\n", "non-negative"},
		{"reserved role", "  vassals: [{role: operator, workload: worker}]\n", "invalid role"},
		{"at role", "  vassals: [{role: \"@x\", workload: worker}]\n", "invalid role"},
		{"duplicate role", "  vassals: [{role: r, workload: worker}, {role: r, workload: worker}]\n", `duplicate vassal role "r"`},
		{"unknown workload", "  vassals: [{role: r, workload: ghost}]\n", `workload "ghost" not found`},
		{"bad activation", "  vassals: [{role: r, workload: worker, activation: sometimes}]\n", "invalid activation"},
		{"unknown message target", "  vassals: [{role: r, workload: worker, canMessage: [nobody]}]\n", `canMessage target "nobody" not found`},
		{"sandbox to runtime", "  vassals: [{role: s, workload: sandboxed, canMessage: [r]}, {role: r, workload: worker}]\n", "sandbox workload cannot message runtime vassal"},
		{"sandbox to all with runtime peer", "  vassals: [{role: s, workload: sandboxed, canMessage: [all]}, {role: r, workload: worker}]\n", "via all"},
		{"sandbox to all among sandboxes", "  vassals: [{role: s, workload: sandboxed, canMessage: [all, operator]}]\n", ""},
		{"runtime to sandbox", "  vassals: [{role: r, workload: worker, canMessage: [s]}, {role: s, workload: sandboxed}]\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := LoadPath(writeTempManifest(t, workers+conclaveDoc(tc.spec)))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadPath = %v", err)
				}
				if tc.name == "default activation" && b.Conclaves["council"].Spec.Vassals[0].Activation != "on-mention" {
					t.Fatalf("activation = %q, want on-mention", b.Conclaves["council"].Spec.Vassals[0].Activation)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LoadPath = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
