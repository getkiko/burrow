package tunnel

import (
	"log"

	"github.com/getkiko/burrow"
)

// LogObserver writes state changes to the standard logger; the CLI uses it.
//
// TCP statistics are not printed here: the SDK's InfoLog already prints the
// same samples, and repeating them would only flood the log.
type LogObserver struct{}

func (LogObserver) OnStateChange(status Status) {
	switch status.State {
	case StateConnected:
		log.Printf("[state] connected local=%s remote=%s tunnels=%d",
			status.LocalAddr, status.RemoteAddr, status.SessionCount)
	case StatePunchFailed:
		log.Printf("[state] punch failed error=%s", status.LastError)
	case StatePunching:
		log.Printf("[state] punching room=%s", status.Room)
	case StateIdle:
		log.Printf("[state] idle")
	}
}

func (LogObserver) OnStats(burrow.TCPStats) {}
