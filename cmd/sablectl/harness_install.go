package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/shadewright-labs/sablecourt/internal/harnessimages"
	"github.com/shadewright-labs/sablecourt/internal/runtime/docker"
	"github.com/spf13/cobra"
)

// fetchTool and newToolDownloader are replaced in tests, so no test reaches the network.
var (
	fetchTool         = harnessimages.FetchTool
	newToolDownloader = func() harnessimages.Downloader { return harnessimages.HTTPDownloader{} }
)

// councilHarnessConfig is the Docker runtime view of one harness definition.
// The daemon builds the same view in cmd/sablecourtd.
func councilHarnessConfig(def harnessimages.Definition) docker.CouncilHarness {
	h := docker.CouncilHarness{Image: def.Image, LoginVolume: def.Volume, LoginFiles: def.LoginFiles}
	if t := def.Tool; t != nil {
		h.Tool = &docker.CouncilTool{Volume: t.Volume, Binary: t.Binary, Version: t.Version, SHA256: t.SHA256, InstallCommand: harnessimages.InstallCommand(def.Role)}
	}
	return h
}

func installHarnessCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install <role>",
		Short: "Install the pinned claude or agy tool into its harness tool volume",
		Long: "Download the pinned vendor tool for claude or agy from the vendor's own download host, check its digest,\n" +
			"and install it into the harness tool volume, which harness containers mount read-only.\n" +
			"The codex image carries its tool, so codex needs no install. The login command runs this step when needed.",
		ValidArgs: []string{"claude", "agy"},
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.ExactArgs(1)(cmd, args); err != nil {
				return err
			}
			_, err := harnessimages.Lookup(args[0])
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			def, err := harnessimages.Lookup(args[0])
			if err != nil {
				return err
			}
			runtime, err := newLoginRuntime(def)
			if err != nil {
				return err
			}
			return installHarnessTool(cmd.Context(), def, runtime, cmd.ErrOrStderr())
		},
	}
}

// installHarnessTool downloads and checks the pinned tool on this machine,
// then streams it into the tool volume through the install container.
func installHarnessTool(ctx context.Context, def harnessimages.Definition, runtime loginRuntime, errOut io.Writer) error {
	if def.Tool == nil {
		return fmt.Errorf("the %s image carries its tool; nothing to install", def.Role)
	}
	fmt.Fprintf(errOut, "Downloading %s %s from %s\n", def.Tool.Binary, def.Tool.Version, def.Tool.URL)
	tool, err := fetchTool(ctx, newToolDownloader(), def.Role)
	if err != nil {
		return fmt.Errorf("install %s: %w", def.Role, err)
	}
	defer func() { _ = tool.Close() }()
	f, err := tool.Open()
	if err != nil {
		return fmt.Errorf("install %s: %w", def.Role, err)
	}
	defer func() { _ = f.Close() }()
	if err := runtime.InstallCouncilTool(ctx, def.Role, f); err != nil {
		if docker.IsNotFound(err) {
			return fmt.Errorf("image for %s is missing: run %s", def.Role, harnessimages.BuildCommand(def.Role))
		}
		var fault *docker.HarnessError
		if errors.As(err, &fault) && fault.Fault == docker.FaultToolInvalid {
			return fmt.Errorf("volume %s exists but is not a local Sablecourt tool volume for %s (driver, options or labels differ): remove it by hand and run this command again", def.Tool.Volume, def.Role)
		}
		return fmt.Errorf("install %s into volume %s: %w", def.Role, def.Tool.Volume, err)
	}
	fmt.Fprintf(errOut, "Installed %s %s (SHA-256 checked) into volume %s\n", def.Tool.Binary, def.Tool.Version, def.Tool.Volume)
	return nil
}
