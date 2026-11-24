package main

import (
	"net/http"

	"github.com/shadewright-labs/sablecourt/internal/conclave"
	"github.com/shadewright-labs/sablecourt/internal/config"
	"github.com/shadewright-labs/sablecourt/internal/controlplane"
	"github.com/shadewright-labs/sablecourt/internal/integrations"
	"github.com/shadewright-labs/sablecourt/internal/secret"
	"github.com/shadewright-labs/sablecourt/internal/store"
)

func incidentHandler(st store.Store, secrets secret.SecretStore, getBundle func() *config.Bundle, broker *conclave.Broker, reconciler *controlplane.Reconciler, defaultTemplate string) http.Handler {
	return integrations.NewShadewright(integrations.ShadewrightOptions{
		Store:           st,
		Secrets:         secrets,
		GetBundle:       getBundle,
		Broker:          broker,
		Reconciler:      reconciler,
		DefaultTemplate: defaultTemplate,
	}).IncidentHandler()
}
