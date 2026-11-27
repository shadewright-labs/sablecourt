//go:build !race

package redact

// raceEnabled is false when the test binary is built without -race.
const raceEnabled = false
