//go:build !race

package docker

// raceEnabled is false when the test binary is built without -race.
const raceEnabled = false
