package conclave

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/contextpack"
	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

// CouncilSnapshot must come from one consistent read of a room and its pinned policy.
// Turns contains one record per reserved seat and round, including retries.
type CouncilSnapshot struct {
	Session              domain.ConclaveSession
	Members              []CouncilMember
	Question             string
	Decrees              []domain.ConclaveMessage
	MaxRounds, MaxStarts int
	Deadline             time.Time
	Sequential           bool
	Turns                []CouncilTurn
}

type CouncilMember = store.CouncilMember
type CouncilTurn = store.CouncilTurn

type CouncilStep struct {
	Action, Reason string
	Round          int
	Starts         []CouncilStart
}

type CouncilStart struct {
	Role    string
	Attempt int
	Packet  contextpack.Packet
}

// Next proposes starts from committed facts. The dispatcher must reserve each
// (room, round, role, attempt) and check the Wards again in the task transaction.
func (c CouncilSnapshot) Next(now time.Time) (CouncilStep, error) {
	if c.Session.ID == "" || c.Session.CreatedAt.IsZero() ||
		now.Before(c.Session.CreatedAt) || !c.Deadline.After(c.Session.CreatedAt) {
		return CouncilStep{}, errors.New("invalid council identity or deadline")
	}
	switch c.Session.State {
	case domain.ConclaveStateCompleted, domain.ConclaveStateCancelled:
		return CouncilStep{Action: "closed", Reason: c.Session.WardReason}, nil
	case domain.ConclaveStateInputRequired, domain.ConclaveStateActive:
	default:
		return CouncilStep{}, errors.New("unknown council state")
	}
	if !now.Before(c.Deadline) {
		return CouncilStep{Action: "cancel", Reason: "deadline"}, nil
	}
	rounds, remaining, err := c.indexTurns()
	if err != nil {
		return CouncilStep{}, err
	}
	if c.Session.State == domain.ConclaveStateInputRequired {
		if c.Session.WardReason == "" {
			return CouncilStep{}, errors.New("paused council has no Ward reason")
		}
		return CouncilStep{Action: "wait", Reason: c.Session.WardReason}, nil
	}
	if c.Session.WardReason != "" {
		return CouncilStep{}, errors.New("active council has an unresolved Ward")
	}
	for round := 1; ; round++ {
		turns := rounds[round]
		complete, unanimous := true, true
		live := false
		step := CouncilStep{Action: "wait", Round: round}
		for _, m := range c.Members {
			role := m.Vassal.Role
			t, started := turns[role]
			if t.Answer != nil {
				unanimous = unanimous && t.NothingToAdd
				continue
			}
			complete = false
			retryable := !started || t.State == domain.TaskStateFailed ||
				t.State == domain.TaskStateCancelled || t.State == domain.TaskStateCompleted
			if !retryable {
				live = true
			}
			if retryable && len(step.Starts) < remaining {
				packet, err := c.packet(round, m, rounds)
				if err != nil {
					return CouncilStep{}, err
				}
				step.Action = "start"
				step.Starts = append(step.Starts, CouncilStart{Role: role, Attempt: t.Starts + 1, Packet: packet})
			}
			if c.Sequential {
				break
			}
		}
		switch {
		case complete && unanimous:
			return CouncilStep{Action: "close", Round: round, Reason: "nothing_to_add"}, nil
		case complete && round == c.MaxRounds:
			return CouncilStep{Action: "close", Round: round, Reason: "round_cap"}, nil
		case complete:
			continue
		case remaining == 0 && !live:
			return CouncilStep{Action: "close", Round: round, Reason: "start_cap"}, nil
		default:
			return step, nil
		}
	}
}

type turnsByRound map[int]map[string]CouncilTurn

func (c CouncilSnapshot) indexTurns() (turnsByRound, int, error) {
	if c.MaxRounds < 1 || c.MaxStarts < 1 || len(c.Members) == 0 || strings.TrimSpace(c.Question) == "" || c.Turns == nil {
		return nil, 0, errors.New("missing council policy or turn evidence")
	}
	members := map[string]bool{}
	for _, m := range c.Members {
		role := m.Vassal.Role
		if members[role] || !config.ValidVassalRole(role) || (m.Zone != domain.ZoneRuntime && m.Zone != domain.ZoneSandbox) {
			return nil, 0, errors.New("invalid council member")
		}
		members[role] = true
	}
	validRecipient := func(role string) bool { return role == "all" || role == "operator" || members[role] }
	for _, d := range c.Decrees {
		if d.ID <= 0 || d.ConclaveID != c.Session.ID || d.TaskID != "" || d.FromRole != "operator" ||
			(d.Kind != "decree" && d.Kind != "decision") || !validRecipient(d.ToRole) ||
			len(d.Body) > maxMessageBody || strings.TrimSpace(d.Body) == "" {
			return nil, 0, errors.New("invalid operator decree")
		}
	}
	remaining := c.MaxStarts
	rounds := turnsByRound{}
	tasks := map[string]bool{}
	for _, t := range c.Turns {
		if !members[t.Role] || t.Round < 1 || t.Round > c.MaxRounds ||
			t.Starts < 1 || t.Starts > remaining || t.TaskID == "" || tasks[t.TaskID] {
			return nil, 0, errors.New("invalid council turn reservation")
		}
		if rounds[t.Round] == nil {
			rounds[t.Round] = map[string]CouncilTurn{}
		}
		if _, ok := rounds[t.Round][t.Role]; ok {
			return nil, 0, errors.New("duplicate council turn")
		}
		switch t.State {
		case domain.TaskStateSubmitted, domain.TaskStateWorking, domain.TaskStateSuspended,
			domain.TaskStateInputRequired, domain.TaskStateCompleted, domain.TaskStateFailed, domain.TaskStateCancelled:
		default:
			return nil, 0, errors.New("unknown council task state")
		}
		if a := t.Answer; a != nil {
			switch a.Kind {
			case "review", "proposal", "question", "blocker":
			default:
				return nil, 0, errors.New("unexpected council answer kind")
			}
			if t.State != domain.TaskStateCompleted || a.ID <= 0 || a.ConclaveID != c.Session.ID ||
				a.FromRole != t.Role || a.TaskID != t.TaskID || !validRecipient(a.ToRole) ||
				len(a.Body) > maxMessageBody || strings.TrimSpace(a.Body) == "" {
				return nil, 0, errors.New("invalid recorded council answer")
			}
		} else if t.NothingToAdd {
			return nil, 0, errors.New("stop vote lacks a recorded answer")
		}
		rounds[t.Round][t.Role] = t
		tasks[t.TaskID] = true
		remaining -= t.Starts
	}
	for round, turns := range rounds {
		gap := false
		unanimous := round > 1
		for _, m := range c.Members {
			if round > 1 {
				previous := rounds[round-1][m.Vassal.Role]
				if previous.Answer == nil {
					return nil, 0, errors.New("turn recorded beyond an incomplete round")
				}
				unanimous = unanimous && previous.NothingToAdd
			}
			t, ok := turns[m.Vassal.Role]
			if c.Sequential && ok && gap {
				return nil, 0, errors.New("turn recorded beyond an incomplete predecessor")
			}
			gap = gap || t.Answer == nil
		}
		if unanimous {
			return nil, 0, errors.New("turn recorded beyond a unanimous stop")
		}
	}
	return rounds, remaining, nil
}

// visible selects the same addressed Decrees and accepted turns for every workload read.
func (c CouncilSnapshot) visible(round int, member CouncilMember, turns turnsByRound) (decrees []domain.ConclaveMessage, peers []CouncilTurn) {
	role := member.Vassal.Role
	for _, d := range c.Decrees {
		if d.ToRole == "all" || d.ToRole == role {
			decrees = append(decrees, d)
		}
	}
	for n := 1; n <= round; n++ {
		if n == round && !c.Sequential {
			break
		}
		for _, sender := range c.Members {
			if n == round && sender.Vassal.Role == role {
				break
			}
			turn := turns[n][sender.Vassal.Role]
			answer := turn.Answer
			if answer == nil || (answer.ToRole != "all" && answer.ToRole != role) {
				continue
			}
			routes := sender.Vassal.CanMessage
			if !slices.Contains(routes, role) && !slices.Contains(routes, "all") {
				continue
			}
			if sender.Zone == domain.ZoneSandbox && member.Zone == domain.ZoneRuntime {
				continue
			}
			peers = append(peers, turn)
		}
	}
	return decrees, peers
}

func (c CouncilSnapshot) packet(round int, member CouncilMember, turns turnsByRound) (contextpack.Packet, error) {
	decrees, peers := c.visible(round, member, turns)
	var instructions []string
	for _, d := range decrees {
		instructions = append(instructions, d.Body)
	}
	var rounds []contextpack.CouncilRound
	for _, peer := range peers {
		if len(rounds) == 0 || rounds[len(rounds)-1].Number != peer.Round {
			rounds = append(rounds, contextpack.CouncilRound{Number: peer.Round})
		}
		r := &rounds[len(rounds)-1]
		r.Answers = append(r.Answers, contextpack.CouncilAnswer{Role: peer.Role, Body: peer.Answer.Body})
	}
	return contextpack.AssembleCouncil(member.RoleCard, c.Question, instructions, rounds)
}
