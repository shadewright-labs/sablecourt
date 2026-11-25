package main

import (
	"cmp"
	"errors"
	"net/http"
	"slices"

	"github.com/shadewright-labs/sablecourt/internal/domain"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func councilDetail(st store.Store, ordinary http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		councils, err := store.Councils(st)
		if errors.Is(err, store.ErrCouncilUnsupported) {
			ordinary(w, r)
			return
		}
		ctx, id := r.Context(), r.PathValue("id")
		// Read the message scan first so the snapshot covers every committed answer in it.
		msgs, msgsErr := st.ListConclaveMessages(ctx, id, 0)
		snap, err := councils.CouncilSnapshot(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			ordinary(w, r)
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot read council detail"})
			return
		}

		relics, relicsErr := st.ListConclaveRelics(ctx, id)
		if msgsErr != nil || relicsErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot read council detail"})
			return
		}
		type message struct {
			domain.ConclaveMessage
			Round int `json:"round,omitempty"`
		}
		answers := make(map[int64]message, len(snap.Attempts))
		for _, attempt := range snap.Attempts {
			if attempt.Answer != nil {
				answers[attempt.Answer.ID] = message{*attempt.Answer, attempt.Round}
			}
		}
		var messages []message
		for _, msg := range msgs {
			if answer, ok := answers[msg.ID]; ok {
				messages = append(messages, answer)
				delete(answers, msg.ID)
			} else {
				messages = append(messages, message{ConclaveMessage: msg})
			}
		}
		for _, answer := range answers {
			messages = append(messages, answer)
		}
		slices.SortFunc(messages, func(a, b message) int { return cmp.Compare(a.ID, b.ID) })
		writeJSON(w, http.StatusOK, map[string]any{
			"conclave": snap.Session, "messages": messages, "relics": relics,
		})
	}
}
