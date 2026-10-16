// Package health aggregates the health of the kernel itself and of the App/Service components it supervises.
//
// health only says whether the nervud kernel process and the components it supervises are running properly right now.
// It is orthogonal to Runtime Availability (the
// available/degraded/fault/offline of Resources/Providers/devices); that authority stays in
// internal/resource (v2+), and this package neither picks it up again nor merges the two into one concept.
//
// health isn't a second source of truth: it only reads the read-only snapshot methods exported by the three existing
// authorities safety/control/service, computing fresh with no state kept between calls, and adds no timers,
// goroutines or polling. v1 adds no wire protocol and does no active probing, so the health
// aggregator can't turn into a new state source or a lifecycle burden.
package health
