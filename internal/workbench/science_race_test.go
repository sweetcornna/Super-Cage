//go:build race

package workbench

// scienceRaceDetector is true under -race, whose instrumentation allocates
// and slows byte loops: allocation bounds do not hold there.
const scienceRaceDetector = true
