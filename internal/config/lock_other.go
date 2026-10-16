//go:build !unix

package config

import "time"

// Lock has no file lock to take here: concurrent writers may lose an update.
func Lock(string, time.Duration) (func(), bool) { return func() {}, true }
