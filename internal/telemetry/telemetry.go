package telemetry

import (
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

const InstrumentationName = "github.com/shadewright-labs/sablecourt"

// NewLogger returns a structured JSON logger for sablecourtd.
func NewLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}

// Tracer returns the package OpenTelemetry tracer from the global provider.
func Tracer() trace.Tracer {
	return otel.Tracer(InstrumentationName)
}
