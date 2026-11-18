package main

import "time"

func sweepLoop(service *signalService, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for now := range ticker.C {
		service.sweep(now)
	}
}
