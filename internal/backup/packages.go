package backup

// Package registries keep their files as content-addressed blobs under
// <data>/packages/blobs/. Uploads in progress live in hidden directories
// (.uploads, .tmp) that snapshots skip.
func init() { RegisterDataDir("packages") }
