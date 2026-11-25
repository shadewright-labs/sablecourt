package docker

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	rt "github.com/shadewright-labs/sablecourt/internal/runtime"
)

type councilEngineAPI interface {
	containerAttacher
	VolumeInspect(context.Context, string, client.VolumeInspectOptions) (client.VolumeInspectResult, error)
	ContainerWait(context.Context, string, client.ContainerWaitOptions) client.ContainerWaitResult
}

var _ rt.CouncilRunner = (*Adapter)(nil)
var _ councilEngineAPI = (*client.Client)(nil)

func (a *Adapter) RunCouncil(ctx context.Context, s rt.CouncilLaunch, record rt.CouncilRecorder) (rt.CouncilResult, error) {
	return a.runCouncil(ctx, s, nil, record)
}

// RunCouncilLogin gives the caller the raw login terminal for the lifetime of
// the session. The callback must honor cancellation. Terminal bytes stay live.
func (a *Adapter) RunCouncilLogin(ctx context.Context, s rt.CouncilLaunch, session func(context.Context, io.Reader, io.Writer) error, record rt.CouncilRecorder) (rt.CouncilResult, error) {
	if session == nil || s.Prompt != "" || s.TaskToken != "" || s.Checkout != (rt.CouncilCheckout{}) || s.Model != "" {
		return rt.CouncilResult{}, errors.New("invalid council login session")
	}
	return a.runCouncil(ctx, s, session, record)
}

const attemptLabel = "sablecourt.io/create-attempt"

func (a *Adapter) runCouncil(parent context.Context, s rt.CouncilLaunch, session func(context.Context, io.Reader, io.Writer) error, record rt.CouncilRecorder) (result rt.CouncilResult, err error) {
	engine, ok := a.Client.(councilEngineAPI)
	if !ok || record == nil {
		return result, errors.New("council requires attach, wait, volume inspect and an evidence recorder")
	}
	ctx, cancel := context.WithTimeout(parent, s.Limits.WallTime)
	defer cancel()
	login := session != nil
	var prompt string
	if !login {
		prompt, err = preparePrompt(s)
		if err != nil {
			return result, err
		}
	}
	o, err := a.councilOptions(ctx, engine, s, login)
	if err != nil {
		return result, err
	}
	result.ContainerName = o.Name
	result.CreateAttempt = rand.Text()
	o.Config.Labels[attemptLabel] = result.CreateAttempt
	created, err := a.Client.ContainerCreate(ctx, o)
	result.ContainerID = created.ID
	defer func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
		defer done()
		if cleanupErr := a.cleanupCouncil(cleanup, &result); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("clean up council container %s: %w", o.Name, cleanupErr))
		}
	}()
	if err != nil {
		return result, fmt.Errorf("create council container: %w", err)
	}
	if created.ID == "" {
		return result, errors.New("Docker returned no container ID")
	}
	if len(created.Warnings) > 0 {
		return result, errors.New("Docker reported a warning applying council configuration")
	}
	// Revalidate once the container pins the named volume so an autocreated or replaced volume cannot start.
	if err := inspectCouncilLoginVolume(ctx, engine, s.Harness, a.CouncilHarnesses[s.Harness].LoginVolume); err != nil {
		return result, err
	}
	stream, err := engine.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		return result, fmt.Errorf("attach council container: %w", err)
	}
	if stream.Conn == nil {
		return result, errors.New("missing council attachment")
	}
	closeStream := sync.OnceFunc(stream.Close)
	defer closeStream()
	stopClose := context.AfterFunc(ctx, closeStream)
	defer stopClose()
	if stream.Reader == nil {
		return result, errors.New("missing council output stream")
	}
	halfClose, ok := stream.Conn.(client.CloseWriter)
	if !login && !ok {
		return result, errors.New("council bootstrap requires half-close support")
	}
	if _, err = a.Client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return result, fmt.Errorf("start council container: %w", err)
	}
	var stdout, stderr councilOutput
	if login {
		err = session(ctx, stream.Reader, stream.Conn)
	} else {
		_, err = io.WriteString(stream.Conn, s.TaskToken+"\x00"+prompt+"\x00")
		if err == nil {
			err = halfClose.CloseWrite()
		}
		if err == nil {
			err = copyDockerStreams(stream.Reader, &stdout, &stderr)
		}
	}
	if err != nil {
		return result, errors.Join(errors.New("council stream failed"), ctx.Err())
	}
	if err = a.councilExit(ctx, engine, &result); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	result.Stdout = stdout.visible(prompt, s.TaskToken)
	result.Stderr = stderr.visible(prompt, s.TaskToken)
	return result, record(ctx, result)
}

// CleanupCouncil retries removal without starting a second workload. A lost
// create response can be reconciled by name only while its attempt label matches.
func (a *Adapter) CleanupCouncil(ctx context.Context, r rt.CouncilResult) error {
	return a.cleanupCouncil(ctx, &r)
}

func (a *Adapter) cleanupCouncil(ctx context.Context, r *rt.CouncilResult) error {
	if r.ContainerID == "" {
		if r.ContainerName == "" || r.CreateAttempt == "" {
			return errors.New("missing council create identity")
		}
		found, err := a.Client.ContainerInspect(ctx, r.ContainerName, client.ContainerInspectOptions{})
		if isMissing(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if found.Container.Config == nil || found.Container.ID == "" {
			return errors.New("incomplete council create evidence")
		}
		if found.Container.Config.Labels[attemptLabel] != r.CreateAttempt {
			return nil
		}
		r.ContainerID = found.Container.ID
	}
	_, err := a.Client.ContainerRemove(ctx, r.ContainerID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if isMissing(err) {
		return nil
	}
	return err
}

func (a *Adapter) councilExit(ctx context.Context, engine councilEngineAPI, result *rt.CouncilResult) error {
	waitCtx, cancelWait := context.WithCancel(ctx)
	defer cancelWait()
	wait := engine.ContainerWait(waitCtx, result.ContainerID, client.ContainerWaitOptions{})
	type waitOutcome struct {
		status  container.WaitResponse
		ok      bool
		errored bool
	}
	done := make(chan waitOutcome, 1)
	go func() {
		select {
		case <-wait.Error:
			done <- waitOutcome{errored: true}
		case status, ok := <-wait.Result:
			done <- waitOutcome{status: status, ok: ok}
		}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case out := <-done:
		if out.errored {
			return errors.New("could not verify council exit")
		}
		if !out.ok || out.status.Error != nil {
			return errors.New("missing council wait status")
		}
		res, err := a.Client.ContainerInspect(ctx, result.ContainerID, client.ContainerInspectOptions{})
		if err != nil {
			return fmt.Errorf("inspect council exit: %w", err)
		}
		st := res.Container.State
		if res.Container.ID != result.ContainerID || st == nil || int64(st.ExitCode) != out.status.StatusCode {
			return errors.New("inconsistent council exit evidence")
		}
		finished, err := verifiedExit(st)
		if err != nil {
			return err
		}
		result.FinishedAt = finished
		result.ExitCode = st.ExitCode
	}
	return nil
}
