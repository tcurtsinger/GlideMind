// Package exit defines glm's process exit codes — part of the CLI contract
// (DESIGN.md §8). Every error surface maps onto these.
package exit

const (
	OK       = 0
	Usage    = 1 // bad flags, bad config, missing profile
	Auth     = 2 // 401/403 from the instance
	API      = 3 // other non-2xx API failures
	Network  = 4 // DNS/TLS/timeout/transport failures
	NotFound = 5 // 404: record or table not found
)
