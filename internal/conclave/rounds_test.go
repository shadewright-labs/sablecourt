package conclave

import (
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/domain"
)

var opened = time.Unix(100, 0).UTC()

func testCouncil() CouncilSnapshot {
	c := CouncilSnapshot{
		Session:  domain.ConclaveSession{ID: "room", State: domain.ConclaveStateActive, CreatedAt: opened},
		Question: "What needs review?", MaxRounds: 2, MaxStarts: 6,
		Deadline: opened.Add(15 * time.Minute), Turns: []CouncilTurn{},
	}
	for _, role := range []string{"reader", "writer", "critic"} {
		c.Members = append(c.Members, CouncilMember{Vassal: domain.ConclaveVassal{Role: role, CanMessage: []string{"all"}}, Zone: domain.ZoneRuntime})
	}
	return c
}

func recorded(round int, role, body string) CouncilTurn {
	id := fmt.Sprintf("task-%d-%s", round, role)
	return CouncilTurn{
		Round: round, Role: role, Starts: 1, TaskID: id, State: domain.TaskStateCompleted,
		Answer: &domain.ConclaveMessage{
			ID:         int64(round*3 + slices.Index([]string{"reader", "writer", "critic"}, role)),
			ConclaveID: "room", TaskID: id, FromRole: role, ToRole: "all", Kind: "review", Body: body,
		},
	}
}

func TestCouncilRoundsWithStubCLI(t *testing.T) {
	for _, seq := range []bool{false, true} {
		t.Run(fmt.Sprint(seq), func(t *testing.T) {
			c := testCouncil()
			c.Sequential = seq
			var calls []string
			for range 8 {
				step, err := c.Next(opened)
				if err != nil {
					t.Fatal(err)
				}
				if step.Action == "close" {
					if step.Reason != "round_cap" {
						t.Fatal(step)
					}
					break
				}
				width := 3
				if seq {
					width = 1
				}
				if step.Action != "start" || len(step.Starts) != width {
					t.Fatalf("step = %+v", step)
				}
				for _, start := range step.Starts {
					md := start.Packet.Markdown
					if !strings.Contains(md, c.Question) {
						t.Fatal("missing question")
					}
					if step.Round == 2 {
						for _, m := range c.Members {
							if !strings.Contains(md, "answer-1-"+m.Vassal.Role) {
								t.Fatalf("missing earlier answer for %s", m.Vassal.Role)
							}
						}
						if !strings.Contains(md, "Review recovery.") {
							t.Fatal("missing decree")
						}
					}
					for _, turn := range c.Turns {
						if turn.Round == step.Round && strings.Contains(md, turn.Answer.Body) != seq {
							t.Fatalf("wrong visibility for %s", turn.Role)
						}
					}
					body := fmt.Sprintf("answer-%d-%s", step.Round, start.Role)
					out, err := exec.CommandContext(t.Context(), "printf", "%s", body).Output()
					if err != nil {
						t.Fatal(err)
					}
					calls = append(calls, body)
					c.Turns = append(c.Turns, recorded(step.Round, start.Role, string(out)))
				}
				if len(calls) == 3 {
					c.Decrees = append(c.Decrees, domain.ConclaveMessage{ID: 20, ConclaveID: "room", FromRole: "operator", ToRole: "all", Kind: "decree", Body: "Review recovery."})
				}
			}
			if !slices.Equal(calls, []string{"answer-1-reader", "answer-1-writer", "answer-1-critic", "answer-2-reader", "answer-2-writer", "answer-2-critic"}) {
				t.Fatal(calls)
			}
			step, err := c.Next(opened)
			if err != nil || step.Action != "close" {
				t.Fatalf("restart at cap = %+v, %v", step, err)
			}
		})
	}
}

func TestCouncilBarrier(t *testing.T) {
	for _, tc := range []struct {
		name           string
		change         func(*CouncilSnapshot)
		action, reason string
		attempt        int
	}{
		{"working", func(c *CouncilSnapshot) { c.Turns[0].State = domain.TaskStateWorking; c.Turns[0].Answer = nil }, "wait", "", 0},
		{"silent", func(c *CouncilSnapshot) { c.Turns[0].Answer = nil }, "start", "", 2},
		{"silent sequential", func(c *CouncilSnapshot) {
			c.Sequential = true
			c.Turns = c.Turns[:1]
			c.Turns[0].Answer = nil
		}, "start", "", 2},
		{"failed", func(c *CouncilSnapshot) { c.Turns[0].State = domain.TaskStateFailed; c.Turns[0].Answer = nil }, "start", "", 2},
		{"timeout", func(c *CouncilSnapshot) { c.Turns[0].State = domain.TaskStateCancelled; c.Turns[0].Answer = nil }, "start", "", 2},
		{"silent at cap", func(c *CouncilSnapshot) { c.MaxStarts = 3; c.Turns[0].Answer = nil }, "close", "start_cap", 0},
		{"exhausted", func(c *CouncilSnapshot) {
			c.MaxStarts = 3
			c.Turns[0].State = domain.TaskStateFailed
			c.Turns[0].Answer = nil
		}, "close", "start_cap", 0},
		{"unanimous", func(c *CouncilSnapshot) {
			for i := range c.Turns {
				c.Turns[i].NothingToAdd = true
			}
		}, "close", "nothing_to_add", 0},
		{"ward", func(c *CouncilSnapshot) {
			c.Session.State = domain.ConclaveStateInputRequired
			c.Session.WardReason = "max_runs"
		}, "wait", "max_runs", 0},
		{"deadline", func(c *CouncilSnapshot) { c.Deadline = opened.Add(time.Second) }, "cancel", "deadline", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testCouncil()
			for _, m := range c.Members {
				c.Turns = append(c.Turns, recorded(1, m.Vassal.Role, "answer"))
			}
			tc.change(&c)
			step, err := c.Next(opened.Add(time.Second))
			if err != nil || step.Action != tc.action || step.Reason != tc.reason {
				t.Fatalf("step = %+v, %v", step, err)
			}
			if tc.attempt > 0 {
				if step.Round != 1 || len(step.Starts) != 1 || step.Starts[0].Role != "reader" || step.Starts[0].Attempt != tc.attempt {
					t.Fatalf("retry = %+v", step)
				}
			} else if len(step.Starts) > 0 {
				t.Fatal("unexpected starts")
			}
		})
	}
}

func TestCouncilParallelRetryPacket(t *testing.T) {
	c := testCouncil()
	for _, m := range c.Members {
		c.Turns = append(c.Turns, recorded(1, m.Vassal.Role, "earlier answer from "+m.Vassal.Role))
	}
	c.Turns = append(c.Turns,
		recorded(2, "reader", "same-round answer"),
		CouncilTurn{Round: 2, Role: "writer", Starts: 1, TaskID: "task-2-writer", State: domain.TaskStateFailed},
		CouncilTurn{Round: 2, Role: "critic", Starts: 1, TaskID: "task-2-critic", State: domain.TaskStateWorking},
	)
	c.MaxStarts++
	step, err := c.Next(opened)
	if err != nil || step.Action != "start" || step.Round != 2 || len(step.Starts) != 1 ||
		step.Starts[0].Role != "writer" || step.Starts[0].Attempt != 2 {
		t.Fatalf("retry = %+v, %v", step, err)
	}
	md := step.Starts[0].Packet.Markdown
	for _, m := range c.Members {
		if !strings.Contains(md, "earlier answer from "+m.Vassal.Role) {
			t.Errorf("retry packet lacks prior answer from %s", m.Vassal.Role)
		}
	}
	if strings.Contains(md, "same-round answer") {
		t.Error("retry packet contains a same-round answer")
	}
}

func TestCouncilEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*CouncilSnapshot)
	}{
		{"wrong answer kind", func(c *CouncilSnapshot) { c.Turns[0].Answer.Kind = "decree" }},
		{"oversize answer", func(c *CouncilSnapshot) { c.Turns[0].Answer.Body = strings.Repeat("x", maxMessageBody+1) }},
		{"unknown recipient", func(c *CouncilSnapshot) { c.Turns[0].Answer.ToRole = "missing" }},
		{"unknown decree recipient", func(c *CouncilSnapshot) {
			c.Decrees = []domain.ConclaveMessage{{ID: 2, ConclaveID: "room", FromRole: "operator", Kind: "decree", Body: "instruction"}}
		}},
		{"unread turns", func(c *CouncilSnapshot) { c.Turns = nil }},
		{"zero rounds", func(c *CouncilSnapshot) { c.MaxRounds = 0 }},
		{"negative starts", func(c *CouncilSnapshot) { c.MaxStarts = -1 }},
		{"ward without reason", func(c *CouncilSnapshot) { c.Session.State = domain.ConclaveStateInputRequired }},
		{"ward with unread turns", func(c *CouncilSnapshot) {
			c.Session.State = domain.ConclaveStateInputRequired
			c.Session.WardReason = "max_runs"
			c.Turns = nil
		}},
		{"ward with invalid ceiling", func(c *CouncilSnapshot) {
			c.Session.State = domain.ConclaveStateInputRequired
			c.Session.WardReason = "max_runs"
			c.MaxRounds = 0
		}},
		{"missing seat", func(c *CouncilSnapshot) { c.Members = nil }},
		{"unknown zone", func(c *CouncilSnapshot) { c.Members[0].Zone = "" }},
		{"duplicate", func(c *CouncilSnapshot) { c.Turns = append(c.Turns, c.Turns[0]) }},
		{"future round", func(c *CouncilSnapshot) { c.Turns[0].Round = 2 }},
		{"round after unanimous stop", func(c *CouncilSnapshot) {
			c.Turns = nil
			for _, m := range c.Members {
				turn := recorded(1, m.Vassal.Role, "nothing to add")
				turn.NothingToAdd = true
				c.Turns = append(c.Turns, turn)
			}
			c.Turns = append(c.Turns, CouncilTurn{Round: 2, Role: "reader", Starts: 1, TaskID: "task-2-reader", State: domain.TaskStateWorking})
		}},
		{"sequential gap", func(c *CouncilSnapshot) { c.Sequential = true; c.Turns[0] = recorded(1, "writer", "answer") }},
		{"wrong task", func(c *CouncilSnapshot) { c.Turns[0].Answer.TaskID = "foreign" }},
		{"wrong room", func(c *CouncilSnapshot) { c.Turns[0].Answer.ConclaveID = "foreign" }},
		{"unattributed stop", func(c *CouncilSnapshot) { c.Turns[0].Answer = nil; c.Turns[0].NothingToAdd = true }},
		{"peer decree", func(c *CouncilSnapshot) { c.Decrees = []domain.ConclaveMessage{*c.Turns[0].Answer} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testCouncil()
			c.Turns = append(c.Turns, recorded(1, "reader", "answer"))
			tc.change(&c)
			step, err := c.Next(opened)
			if err == nil || len(step.Starts) != 0 {
				t.Fatalf("invalid evidence accepted: %+v, %v", step, err)
			}
		})
	}
}

func TestCouncilRouting(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*CouncilSnapshot)
	}{
		{"directed", func(c *CouncilSnapshot) { c.Turns[0].Answer.ToRole = "critic" }},
		{"route removed", func(c *CouncilSnapshot) { c.Members[0].Vassal.CanMessage = []string{"critic"} }},
		{"sandbox to runtime", func(c *CouncilSnapshot) { c.Members[0].Zone = domain.ZoneSandbox }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testCouncil()
			c.Sequential = true
			c.Turns = append(c.Turns, recorded(1, "reader", "restricted answer"))
			tc.change(&c)
			c.Decrees = []domain.ConclaveMessage{{ID: 4, ConclaveID: "room", FromRole: "operator", ToRole: "critic", Kind: "decree", Body: "private instruction"}}
			step, err := c.Next(opened)
			if err != nil || len(step.Starts) != 1 {
				t.Fatalf("step = %+v, %v", step, err)
			}
			p := step.Starts[0].Packet.Markdown
			if step.Starts[0].Role != "writer" || strings.Contains(p, "restricted answer") || strings.Contains(p, "private instruction") {
				t.Fatal(p)
			}
		})
	}
}
