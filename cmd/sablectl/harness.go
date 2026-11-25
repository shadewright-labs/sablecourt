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
	harness := &cobra.Command{Use: "harness", Short: "Build local council harness images"}
	build := &cobra.Command{
		Use:       "build <role>",
		Short:     "Build a pinned harness image and its proxy locally",
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
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "Built %s and %s locally\n", d.Image, harnessimages.ProxyImage())
			return err
		},
	}
	build.Flags().StringVar(&source, "source", ".", "Sablecourt source checkout containing the council proxy")
	harness.AddCommand(build)
	return harness
}
