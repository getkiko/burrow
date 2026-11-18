package burrow

// predictivePortSequence expands the port window scanned by the predictive
// strategy. It is centered on basePort and expands outward in the order
// P, P+1, P-1, P+2, P-2, ..., so the most likely ports are tried first.
// scanRange is the total window width (both sides combined); it is capped
// at predictiveUDPMaxPortRange (4096).
//
// Ports outside 1..65535 are skipped rather than wrapped: on UDP the window
// is centered on a mapping port that was actually observed, so wrap-around
// is never useful, and skipping does not change the center-out order.
func predictivePortSequence(basePort, scanRange int) []int {
	if scanRange < 0 {
		scanRange = 0
	}
	if scanRange > predictiveUDPMaxPortRange {
		scanRange = predictiveUDPMaxPortRange
	}
	ports := make([]int, 0, scanRange+1)
	appendPort := func(p int) {
		if p >= 1 && p <= 65535 {
			ports = append(ports, p)
		}
	}
	appendPort(basePort)
	for offset := 1; len(ports) <= scanRange && offset <= scanRange; offset++ {
		appendPort(basePort + offset)
		if len(ports) > scanRange {
			break
		}
		appendPort(basePort - offset)
	}
	if len(ports) > scanRange+1 {
		ports = ports[:scanRange+1]
	}
	return ports
}
