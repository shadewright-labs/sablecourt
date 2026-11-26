//go:build race

package redact_test

// raceEnabled reports a -race build, whose instrumentation moves values to
// the heap and makes allocation counts meaningless.
const raceEnabled = true
