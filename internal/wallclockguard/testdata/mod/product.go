package fixture

import "time"

// Product code is not a hermetic test and is not scanned.
func Product() { <-time.After(time.Millisecond) }
