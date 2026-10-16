package main

// Windows has no SIGHUP; the daemon polls config.json instead.
func notifyReload(chan struct{}) {}
