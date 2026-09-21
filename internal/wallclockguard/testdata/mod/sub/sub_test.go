package sub

import "time"

func helper() { <-time.After(time.Second) } // VIOLATION: a subpackage is scanned too
