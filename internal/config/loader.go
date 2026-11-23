package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/robfig/cron/v3"
	"sigs.k8s.io/yaml"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

type Bundle struct {
	Workloads      map[string]domain.Workload
	Policies       map[string]domain.Policy
	Budgets        map[string]domain.Budget
	Workflows      map[string]domain.Workflow
	Schedules      map[string]domain.Schedule
	Conclaves      map[string]domain.Conclave
	ManifestDigest string
}

func LoadPath(path string) (*Bundle, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat manifest path %q: %w", path, err)
	}

	var files []string
	if info.IsDir() {
		err = filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(p))
			if ext == ".yaml" || ext == ".yml" {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk manifest dir %q: %w", path, err)
		}
		sort.Strings(files)
	} else {
		files = []string{path}
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no YAML manifest files found in %q", path)
	}

	hasher := sha256.New()
	bundle := &Bundle{
		Workloads: make(map[string]domain.Workload),
		Policies:  make(map[string]domain.Policy),
		Budgets:   make(map[string]domain.Budget),
		Workflows: make(map[string]domain.Workflow),
		Schedules: make(map[string]domain.Schedule),
		Conclaves: make(map[string]domain.Conclave),
	}

	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", file, err)
		}
		hasher.Write([]byte(filepath.Base(file)))
		hasher.Write([]byte{0})
		hasher.Write(raw)
		hasher.Write([]byte{0})

		if err := parseDocuments(file, raw, bundle); err != nil {
			return nil, err
		}
	}

	bundle.ManifestDigest = hex.EncodeToString(hasher.Sum(nil))
	if err := validateBundle(bundle); err != nil {
		return nil, err
	}
	return bundle, nil
}

func parseDocuments(source string, raw []byte, bundle *Bundle) error {
	docs := splitYAMLDocuments(raw)
	for idx, doc := range docs {
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}

		var meta domain.TypeMeta
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			return fmt.Errorf("%s (doc %d): parse type header: %w", source, idx+1, err)
		}
		if meta.APIVersion != domain.APIVersionV1Alpha1 {
			return fmt.Errorf("%s (doc %d): unsupported apiVersion %q (expected %q)", source, idx+1, meta.APIVersion, domain.APIVersionV1Alpha1)
		}

		switch meta.Kind {
		case domain.KindWorkload:
			var w domain.Workload
			if err := yaml.UnmarshalStrict(doc, &w); err != nil {
				return fmt.Errorf("%s (doc %d): decode Workload: %w", source, idx+1, err)
			}
			if strings.TrimSpace(w.Metadata.Name) == "" {
				return fmt.Errorf("%s (doc %d): Workload metadata.name is required", source, idx+1)
			}
			if _, exists := bundle.Workloads[w.Metadata.Name]; exists {
				return fmt.Errorf("%s (doc %d): duplicate Workload %q", source, idx+1, w.Metadata.Name)
			}
			bundle.Workloads[w.Metadata.Name] = w

		case domain.KindPolicy:
			var p domain.Policy
			if err := yaml.UnmarshalStrict(doc, &p); err != nil {
				return fmt.Errorf("%s (doc %d): decode Policy: %w", source, idx+1, err)
			}
			if strings.TrimSpace(p.Metadata.Name) == "" {
				return fmt.Errorf("%s (doc %d): Policy metadata.name is required", source, idx+1)
			}
			if _, exists := bundle.Policies[p.Metadata.Name]; exists {
				return fmt.Errorf("%s (doc %d): duplicate Policy %q", source, idx+1, p.Metadata.Name)
			}
			bundle.Policies[p.Metadata.Name] = p

		case domain.KindBudget:
			var b domain.Budget
			if err := yaml.UnmarshalStrict(doc, &b); err != nil {
				return fmt.Errorf("%s (doc %d): decode Budget: %w", source, idx+1, err)
			}
			if strings.TrimSpace(b.Metadata.Name) == "" {
				return fmt.Errorf("%s (doc %d): Budget metadata.name is required", source, idx+1)
			}
			if _, exists := bundle.Budgets[b.Metadata.Name]; exists {
				return fmt.Errorf("%s (doc %d): duplicate Budget %q", source, idx+1, b.Metadata.Name)
			}
			bundle.Budgets[b.Metadata.Name] = b

		case domain.KindWorkflow:
			var wf domain.Workflow
			if err := yaml.UnmarshalStrict(doc, &wf); err != nil {
				return fmt.Errorf("%s (doc %d): decode Workflow: %w", source, idx+1, err)
			}
			if strings.TrimSpace(wf.Metadata.Name) == "" {
				return fmt.Errorf("%s (doc %d): Workflow metadata.name is required", source, idx+1)
			}
			if _, exists := bundle.Workflows[wf.Metadata.Name]; exists {
				return fmt.Errorf("%s (doc %d): duplicate Workflow %q", source, idx+1, wf.Metadata.Name)
			}
			bundle.Workflows[wf.Metadata.Name] = wf

		case domain.KindSchedule:
			var sc domain.Schedule
			if err := yaml.UnmarshalStrict(doc, &sc); err != nil {
				return fmt.Errorf("%s (doc %d): decode Schedule: %w", source, idx+1, err)
			}
			if strings.TrimSpace(sc.Metadata.Name) == "" {
				return fmt.Errorf("%s (doc %d): Schedule metadata.name is required", source, idx+1)
			}
			if _, exists := bundle.Schedules[sc.Metadata.Name]; exists {
				return fmt.Errorf("%s (doc %d): duplicate Schedule %q", source, idx+1, sc.Metadata.Name)
			}
			bundle.Schedules[sc.Metadata.Name] = sc

		case domain.KindConclave:
			var cc domain.Conclave
			if err := yaml.UnmarshalStrict(doc, &cc); err != nil {
				return fmt.Errorf("%s (doc %d): decode Conclave: %w", source, idx+1, err)
			}
			if strings.TrimSpace(cc.Metadata.Name) == "" {
				return fmt.Errorf("%s (doc %d): Conclave metadata.name is required", source, idx+1)
			}
			if _, exists := bundle.Conclaves[cc.Metadata.Name]; exists {
				return fmt.Errorf("%s (doc %d): duplicate Conclave %q", source, idx+1, cc.Metadata.Name)
			}
			for i := range cc.Spec.Vassals {
				if cc.Spec.Vassals[i].Activation == "" {
					cc.Spec.Vassals[i].Activation = domain.ActivationOnMention
				}
			}
			bundle.Conclaves[cc.Metadata.Name] = cc

		default:
			return fmt.Errorf("%s (doc %d): unsupported kind %q", source, idx+1, meta.Kind)
		}
	}
	return nil
}

func splitYAMLDocuments(raw []byte) [][]byte {
	lines := bytes.Split(raw, []byte("\n"))
	var docs [][]byte
	var current [][]byte
	for _, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if bytes.Equal(trimmed, []byte("---")) {
			if len(current) > 0 {
				docs = append(docs, bytes.Join(current, []byte("\n")))
				current = nil
			}
			continue
		}
		current = append(current, line)
	}
	if len(current) > 0 {
		docs = append(docs, bytes.Join(current, []byte("\n")))
	}
	return docs
}

func validateBundle(bundle *Bundle) error {
	for name, b := range bundle.Budgets {
		if b.Spec.PerTask.MaxInputTokens < 0 || b.Spec.PerTask.MaxOutputTokens < 0 || b.Spec.PerTask.MaxCostUSD < 0 || b.Spec.Daily.MaxCostUSD < 0 {
			return fmt.Errorf("budget %q: limits must be non-negative", name)
		}
		if b.Spec.PerTask.MaxInputTokens == 0 && b.Spec.PerTask.MaxOutputTokens == 0 && b.Spec.PerTask.MaxCostUSD == 0 && b.Spec.Daily.MaxCostUSD == 0 {
			return fmt.Errorf("budget %q: at least one limit must be positive", name)
		}
	}

	for name, p := range bundle.Policies {
		if p.Spec.DefaultEffect != domain.EffectDeny {
			return fmt.Errorf("policy %q: defaultEffect must be %q (got %q)", name, domain.EffectDeny, p.Spec.DefaultEffect)
		}
		for i, r := range p.Spec.Rules {
			if strings.TrimSpace(r.Workload) == "" || strings.TrimSpace(r.Capability) == "" {
				return fmt.Errorf("policy %q rule %d: workload and capability are required", name, i)
			}
			if !isValidEffect(r.Effect) {
				return fmt.Errorf("policy %q rule %d: invalid effect %q", name, i, r.Effect)
			}
		}
	}

	for name, w := range bundle.Workloads {
		if err := validateWorkload(name, w, bundle); err != nil {
			return err
		}
	}

	for name, wf := range bundle.Workflows {
		if wf.Metadata.Zone != "" && wf.Metadata.Zone != domain.ZoneRuntime && wf.Metadata.Zone != domain.ZoneSandbox {
			return fmt.Errorf("workflow %q: invalid zone %q", name, wf.Metadata.Zone)
		}
		if len(wf.Spec.Steps) == 0 {
			return fmt.Errorf("workflow %q: at least one step is required", name)
		}
		for i, step := range wf.Spec.Steps {
			if strings.TrimSpace(step.Name) == "" {
				return fmt.Errorf("workflow %q step %d: name is required", name, i)
			}
			wl, ok := bundle.Workloads[step.Workload]
			if !ok {
				return fmt.Errorf("workflow %q step %q: workload %q not found", name, step.Name, step.Workload)
			}
			if wl.Spec.Class == domain.ClassDeterministicController && (step.Provider != "" || step.Model != "") {
				return fmt.Errorf("workflow %q step %q: deterministic-controller cannot specify provider or model", name, step.Name)
			}
		}
	}

	for name, sc := range bundle.Schedules {
		if _, err := cron.ParseStandard(sc.Spec.Cron); err != nil {
			return fmt.Errorf("schedule %q: invalid cron expression %q: %w", name, sc.Spec.Cron, err)
		}
		switch sc.Spec.TargetKind {
		case "workload":
			if _, ok := bundle.Workloads[sc.Spec.TargetName]; !ok {
				return fmt.Errorf("schedule %q: target workload %q not found", name, sc.Spec.TargetName)
			}
		case "workflow":
			if _, ok := bundle.Workflows[sc.Spec.TargetName]; !ok {
				return fmt.Errorf("schedule %q: target workflow %q not found", name, sc.Spec.TargetName)
			}
		default:
			return fmt.Errorf("schedule %q: targetKind must be workload or workflow (got %q)", name, sc.Spec.TargetKind)
		}
	}

	for name, cc := range bundle.Conclaves {
		if err := validateConclave(name, cc, bundle); err != nil {
			return err
		}
	}
	return nil
}

func validateConclave(name string, cc domain.Conclave, bundle *Bundle) error {
	if len(cc.Spec.Vassals) == 0 {
		return fmt.Errorf("conclave %q: at least one vassal is required", name)
	}
	if cc.Spec.BudgetRef != "" {
		if _, ok := bundle.Budgets[cc.Spec.BudgetRef]; !ok {
			return fmt.Errorf("conclave %q: referenced budget %q not found", name, cc.Spec.BudgetRef)
		}
	}
	if cc.Spec.Wards.MaxMessages < 0 || cc.Spec.Wards.MaxPingPong < 0 {
		return fmt.Errorf("conclave %q: ward limits must be non-negative", name)
	}
	roles := make(map[string]domain.Workload, len(cc.Spec.Vassals))
	for i, v := range cc.Spec.Vassals {
		role := strings.TrimSpace(v.Role)
		if role == "" || role == "operator" || role == "all" || role == "system" {
			return fmt.Errorf("conclave %q vassal %d: invalid role %q", name, i, v.Role)
		}
		if _, dup := roles[role]; dup {
			return fmt.Errorf("conclave %q: duplicate vassal role %q", name, role)
		}
		wl, ok := bundle.Workloads[v.Workload]
		if !ok {
			return fmt.Errorf("conclave %q vassal %q: workload %q not found", name, role, v.Workload)
		}
		if v.Activation != domain.ActivationAlwaysOn && v.Activation != domain.ActivationOnMention {
			return fmt.Errorf("conclave %q vassal %q: invalid activation %q", name, role, v.Activation)
		}
		roles[role] = wl
	}
	for _, v := range cc.Spec.Vassals {
		srcWL := roles[v.Role]
		for _, target := range v.CanMessage {
			if target == "operator" || target == "all" {
				if target == "all" && srcWL.Metadata.Zone == domain.ZoneSandbox {
					for _, other := range cc.Spec.Vassals {
						if roles[other.Role].Metadata.Zone == domain.ZoneRuntime {
							return fmt.Errorf("conclave %q vassal %q: sandbox workload cannot message runtime vassal %q via all", name, v.Role, other.Role)
						}
					}
				}
				continue
			}
			dstWL, ok := roles[target]
			if !ok {
				return fmt.Errorf("conclave %q vassal %q: canMessage target %q not found", name, v.Role, target)
			}
			if srcWL.Metadata.Zone == domain.ZoneSandbox && dstWL.Metadata.Zone == domain.ZoneRuntime {
				return fmt.Errorf("conclave %q vassal %q: sandbox workload cannot message runtime vassal %q", name, v.Role, target)
			}
		}
	}
	return nil
}

func validateWorkload(name string, w domain.Workload, bundle *Bundle) error {
	if w.Metadata.Zone != domain.ZoneRuntime && w.Metadata.Zone != domain.ZoneSandbox {
		return fmt.Errorf("workload %q: metadata.zone must be %q or %q (got %q)", name, domain.ZoneRuntime, domain.ZoneSandbox, w.Metadata.Zone)
	}

	switch w.Spec.Class {
	case domain.ClassCodingAgent:
		if w.Spec.Coding == nil {
			return fmt.Errorf("workload %q: coding config is required for class %q", name, w.Spec.Class)
		}
		if w.Spec.Coding.Harness != "claude" && w.Spec.Coding.Harness != "codex" && w.Spec.Coding.Harness != "agy" {
			return fmt.Errorf("workload %q: unsupported coding harness %q", name, w.Spec.Coding.Harness)
		}
		if strings.TrimSpace(w.Spec.Coding.RepoPath) == "" {
			return fmt.Errorf("workload %q: coding.repoPath is required", name)
		}
	case domain.ClassLongLivedAgent:
		if w.Spec.Coding != nil {
			return fmt.Errorf("workload %q: coding config is only valid for class %q", name, domain.ClassCodingAgent)
		}
	case domain.ClassDeterministicController:
		if w.Spec.Coding != nil {
			return fmt.Errorf("workload %q: coding config is only valid for class %q", name, domain.ClassCodingAgent)
		}
		if w.Spec.Grants.ModelGate.Allowed {
			return fmt.Errorf("workload %q: deterministic-controller must not hold a Model Gate grant", name)
		}
	default:
		return fmt.Errorf("workload %q: invalid class %q", name, w.Spec.Class)
	}

	if w.Spec.Runtime.Type != "docker" && w.Spec.Runtime.Type != "worktree-cli" {
		return fmt.Errorf("workload %q: runtime.type must be docker or worktree-cli (got %q)", name, w.Spec.Runtime.Type)
	}
	if w.Spec.Runtime.Type == "docker" && strings.TrimSpace(w.Spec.Runtime.Image) == "" {
		return fmt.Errorf("workload %q: runtime.image is required for docker runtime", name)
	}

	if w.Metadata.Zone == domain.ZoneSandbox {
		if len(w.Spec.Grants.Actions) > 0 || len(w.Spec.Grants.DelegateTo) > 0 {
			return errors.New("workload " + name + ": sandbox zone workloads cannot hold mutating actions or delegation grants")
		}
	}

	if w.Spec.Grants.ModelGate.Allowed {
		ref := strings.TrimSpace(w.Spec.Grants.ModelGate.BudgetRef)
		if ref == "" {
			return fmt.Errorf("workload %q: grants.modelGate.budgetRef is required when modelGate.allowed is true", name)
		}
		if _, ok := bundle.Budgets[ref]; !ok {
			return fmt.Errorf("workload %q: referenced budget %q not found", name, ref)
		}
	}

	for _, target := range w.Spec.Grants.DelegateTo {
		if target == name {
			return fmt.Errorf("workload %q: cannot delegate to itself", name)
		}
		if _, ok := bundle.Workloads[target]; !ok {
			return fmt.Errorf("workload %q: delegateTo target %q not found", name, target)
		}
	}

	return nil
}

func isValidEffect(e domain.PolicyEffect) bool {
	return e == domain.EffectAllow || e == domain.EffectApprove || e == domain.EffectDeny
}
