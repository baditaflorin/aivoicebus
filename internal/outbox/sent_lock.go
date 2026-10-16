package outbox

import (
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
)

// lockSent takes the exclusive lock on the file at path (config.Lock): two sessions sending at once
// each read the memory, add their message and write it back, and without the lock the second write
// would drop the first's entry. It gives up after wait (ok false), because remembering a send is
// never worth holding one up.
func lockSent(path string, wait time.Duration) (unlock func(), ok bool) {
	return config.Lock(path, wait)
}
