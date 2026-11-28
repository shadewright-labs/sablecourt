package modelfamily

import "errors"

// ChainEntry is one model run that fed the change under review in a workflow
// run: the author step, a fast step or a step with no role before the
// verifier, every retry of those, every fix round, and each model the Model
// Gate admitted for those tasks. A verifier never joins the chain, and a
// deterministic step adds nothing.
type ChainEntry struct {
	Step   string
	TaskID string
	Model  string
	// Family is the operator's family on the route the entry ran, if any.
	Family string
}

// Verifier is the resolved route of a verifier step at dispatch.
type Verifier struct {
	Step string
	// Deterministic is true for a step on a deterministic-controller
	// workload, which runs no model and is outside the family rule.
	Deterministic bool
	Model         string
	// Family is the operator's family on the verifier's route, if any.
	Family string
}

// CheckVerifier allows (nil) or refuses (*Refusal) the dispatch of a verifier
// step. It refuses when the verifier's family is unknown, when the chain is
// empty or holds an unknown family, and when the verifier's family appears in
// the chain. It is pure: it reads nothing but its arguments and changes
// neither.
func CheckVerifier(chain []ChainEntry, v Verifier) error {
	if v.Deterministic {
		return nil
	}
	vf, err := Classify(v.Model, v.Family)
	if err != nil {
		return refuse(ReasonOf(err), "verifier step %q: %s", v.Step, detail(err))
	}
	if len(chain) == 0 {
		return refuse(ReasonChainEmpty, "verifier step %q: no earlier step in the run ran a model", v.Step)
	}
	for _, e := range chain {
		if _, err := Classify(e.Model, e.Family); err != nil {
			return refuse(ReasonChainUnknown, "verifier step %q: chain step %q task %q: %s", v.Step, e.Step, e.TaskID, detail(err))
		}
	}
	for _, e := range chain {
		if ef, _ := Classify(e.Model, e.Family); ef == vf {
			return refuse(ReasonSameFamily, "verifier step %q runs %s, which step %q task %q already ran", v.Step, vf, e.Step, e.TaskID)
		}
	}
	return nil
}

// detail returns the text of a refusal without its reason code.
func detail(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Detail
	}
	return err.Error()
}
