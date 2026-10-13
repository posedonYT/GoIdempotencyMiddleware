// Package idempotency provides HTTP middleware that deduplicates mutating
// requests using an Idempotency-Key header and replays stored responses.
//
// See DESIGN.md and idempotency-plan.md for implementation stages.
package idempotency
