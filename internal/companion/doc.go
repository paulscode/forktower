// Package companion observes the health of co-resident processes, such as the
// second Bitcoin node and the watchtower.
//
// It reports on them; it does not manage them. Process supervision belongs to
// whatever init system the deployment already has.
package companion
