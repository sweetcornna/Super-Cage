package policy

// MinBattery is the lowest battery charge, in percent, at which an
// unattended update may run on battery power. An update stops the
// environment, replaces the application and starts everything again; it
// should not drain a laptop that nobody is watching.
const MinBattery = 50

// HostState is the host's power and display state, read once for an
// unattended decision. A nil *HostState means it could not be read, which
// is "not now".
type HostState struct {
	// DarkWake is a maintenance wake (Power Nap, a timer) with no display:
	// the machine may go back to sleep at any moment.
	DarkWake bool
	// LidClosed is a closed laptop without an external display; it stays
	// awake only because something holds it awake.
	LidClosed bool
	// OnBattery and Battery describe the power source; Battery is a
	// percentage and is meaningful only on battery power.
	OnBattery bool
	Battery   int
	// DisplayHeld names other processes that keep the display awake: a
	// video, a call, a presentation.
	DisplayHeld []string
}
