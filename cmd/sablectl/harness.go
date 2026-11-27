package main

import (
	"fmt"

	"github.com/shadewright-labs/sablecourt/internal/harnessimages"
	"github.com/spf13/cobra"
)

func buildHarnessCmd(docker harnessimages.Docker) *cobra.Command {
	return buildHarnessCmdWithCompiler(docker, nil)
}

func buildHarnessCmdWithCompiler(docker harnessimages.Docker, compiler harnessimages.ProxyCompiler) *cobra.Command {
	var source string
	harness := &cobra.Command{Use: "harness", Short: "Build local council harness images and log in to them"}
	build := &cobra.Command{
		Use:   "build <role>",
		Short: "Build a pinned harness image and its proxy",
		Long: "Build a pinned harness image and its proxy from the embedded Dockerfile.\n" +
			"The proxy is compiled on this machine; the Docker engine that the docker command selects (DOCKER_HOST or DOCKER_CONTEXT) builds both images.\n" +
			"The codex image includes the Codex CLI. The claude and agy images carry no vendor tool:\n" +
			"sablectl harness login installs the pinned tool into its own volume, as for a published image.",
		ValidArgs: []string{"claude", "codex", "agy"},
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.ExactArgs(1)(cmd, args); err != nil {
				return err
			}
			_, err := harnessimages.Lookup(args[0])
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			selectedCompiler := compiler
			if selectedCompiler == nil {
				selectedCompiler = harnessimages.LocalProxyCompiler{Source: source}
			}
			if err := harnessimages.Build(cmd.Context(), docker, selectedCompiler, args[0]); err != nil {
				return err
			}
			d, _ := harnessimages.Lookup(args[0])
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Built %s and %s with the configured Docker engine\n", d.Image, harnessimages.ProxyImage()); err != nil {
				return err
			}
			if d.Tool != nil {
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "The image carries no %s binary: %s installs the pinned version %s\n", d.Tool.Binary, harnessimages.LoginCommand(d.Role), d.Tool.Version)
				return err
			}
			return nil
		},
	}
	build.Flags().StringVar(&source, "source", ".", "Sablecourt source checkout containing the council proxy")
	harness.AddCommand(build, loginHarnessCmd(), installHarnessCmd())
	return harness
}
