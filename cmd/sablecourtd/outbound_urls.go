package main

import "github.com/shadewright-labs/sablecourt/internal/outbound"

// checkOutboundURLs refuses, before the daemon creates any state, an outbound
// URL from a flag or the environment that would send traffic in plain text
// off the machine (outbound.Check). gatewayField names where the model
// gateway URL came from, so the error points at the flag or the variable.
func checkOutboundURLs(cfg daemonConfig, gatewayField string) error {
	if cfg.modelGatewayURL != "" {
		if err := outbound.Check(gatewayField, cfg.modelGatewayURL); err != nil {
			return err
		}
	}
	if cfg.manifestsGitRepo != "" {
		if err := outbound.CheckGitRemote("--manifests-git-repo", cfg.manifestsGitRepo); err != nil {
			return err
		}
	}
	return nil
}
