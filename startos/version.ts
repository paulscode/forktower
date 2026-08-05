/**
 * The version of Forktower this package ships.
 *
 * One constant, used twice: as the package's own version, and as the build
 * argument that becomes `forktowerd --version`. They were separate once, and the
 * binary inside the first packed s9pk reported itself as `dev` — which would
 * have been the version a user quoted in a bug report.
 */
export const appVersion = '0.5.0'

/**
 * The StartOS package revision.
 *
 * Bumped when the packaging changes but Forktower itself has not — a corrected
 * description, a new health check — so the platform can offer the update without
 * implying the daemon changed.
 */
export const packageRevision = 8
