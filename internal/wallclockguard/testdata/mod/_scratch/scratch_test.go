package scratch

import "time"

// The go command never compiles a directory whose name starts with "_", so
// neither does the guard.
func helper() { <-time.After(time.Second) }
