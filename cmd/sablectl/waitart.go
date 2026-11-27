package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/shadewright-labs/sablecourt/internal/waitart"
)

// noArtFlag is the persistent flag that turns the wait animation off.
const noArtFlag = "no-art"

// artConfig returns the wait animation settings for cmd: the --no-art flag,
// with the environment read when the animation starts. A command built
// without the root command has no such flag and keeps the defaults.
func artConfig(cmd *cobra.Command) waitart.Config {
	noArt, _ := cmd.Flags().GetBool(noArtFlag)
	return waitart.Config{NoArt: noArt}
}

// startWait plays a scene for the kind of wait on the command's stderr. It
// returns nil, which is safe to use, when the animation is off.
func startWait(cmd *cobra.Command, kind waitart.Kind, status string) *waitart.Player {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return artConfig(cmd).Start(ctx, cmd.ErrOrStderr(), waitart.ForKind(kind), status)
}

// writeWhole writes a message line and reports whether all of it reached w.
// The animation starts only after a complete line: a short or failed write
// leaves a partial line on the screen, and a frame must not land in it.
func writeWhole(w io.Writer, line string) bool {
	n, err := io.WriteString(w, line)
	return err == nil && n == len(line)
}

// councilStatus is the status line under the council scene, from the round
// state the daemon reports.
func councilStatus(maxRounds int, d councilDetailResponse) string {
	round := 0
	for _, t := range d.Turns {
		round = max(round, t.Round)
	}
	if round == 0 {
		for _, m := range d.Messages {
			round = max(round, m.Round)
		}
	}
	if round == 0 {
		return "Council in session: waiting for the first answers"
	}
	seats, answered := 0, 0
	for _, t := range d.Turns {
		if t.Round != round {
			continue
		}
		seats++
		if strings.EqualFold(strings.TrimSpace(t.Status), "completed") || strings.EqualFold(strings.TrimSpace(t.State), "completed") {
			answered++
		}
	}
	if seats == 0 {
		return fmt.Sprintf("Council in session: round %d of %d", round, maxRounds)
	}
	return fmt.Sprintf("Council in session: round %d of %d, %d of %d seats answered", round, maxRounds, answered, seats)
}
