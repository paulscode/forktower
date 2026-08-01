import { HealthCheckResult } from '@start9labs/start-sdk/package/lib/health/checkFns'
import { announcedAlertsKept, storeJson } from './fileModels/store.json'
import {
  AlertTier,
  ForktowerAlert,
  dashboardPort,
  notificationLevels,
} from './utils'

/**
 * The wrapper is what notifies, because the daemon cannot.
 *
 * An app container has no path to the platform's notification system — no
 * `start-cli` on its PATH, no socket, no mount, and no amount of wanting one
 * changes that. What it does have is an API, so the wrapper reads
 * `GET /api/v1/alerts` and raises what it finds. The daemon's job is to know;
 * this file's job is to carry.
 *
 * Anything read here has already been through the daemon's own de-duplication,
 * so an alert that is "raised" repeatedly arrives as one row with a moving
 * `last_raised_at` rather than as a stream. What is left to avoid is announcing
 * the same row twice across a restart, which is what `store.json` is for.
 */

/** Alerts of at least this tier are announced. */
const announcedTiers: ReadonlyArray<string> = [
  'loss',
  'critical',
  'warning',
  'notice',
]

/** The worst tier standing decides the health dot. */
const failingTiers: ReadonlyArray<string> = ['loss', 'critical']

/**
 * The little of a subcontainer this file needs.
 *
 * Structural rather than the SDK's own type, so this module can be reasoned
 * about — and tested — without standing up a container.
 */
type Container = {
  exec: (
    command: string[],
    ...rest: never[]
  ) => Promise<{ exitCode: number | null; stdout: string | Buffer }>
}

type Effects = {
  notification: {
    create: (o: {
      level: 'success' | 'info' | 'warning' | 'error'
      title: string
      message: string
      data?: string | null
    }) => Promise<null>
  }
}

/**
 * Read the standing alerts, announce the new ones, and report what is left.
 *
 * Runs on the health-check timer. Never throws: a pass that cannot reach the
 * daemon reports that it cannot, which is a truthful health result, and tries
 * again next time. Throwing here would turn a momentary hiccup into a red
 * package.
 */
export async function announceNewAlerts(
  effects: Effects,
  container: Container,
): Promise<HealthCheckResult> {
  const alerts = await readAlerts(container)
  if (alerts === null) {
    return {
      result: 'starting',
      message: 'Waiting for Forktower to answer',
    }
  }

  const standing = alerts.filter(
    (a) => a.acked_at === 0 && announcedTiers.includes(a.tier),
  )

  await raiseUnannounced(effects, standing)

  if (standing.length === 0) {
    return { result: 'success', message: 'Nothing needs your attention' }
  }

  const worst = standing.reduce((a, b) =>
    announcedTiers.indexOf(a.tier) <= announcedTiers.indexOf(b.tier) ? a : b,
  )
  const summary =
    standing.length === 1
      ? worst.subject
      : `${worst.subject} (and ${standing.length - 1} more)`

  // A warning is reported as `loading` rather than `failure` on purpose. In
  // StartOS a red package reads as "this software is broken", and a warning
  // from Forktower usually means the *world* needs attention rather than the
  // daemon — the chains diverging is the program working, not failing.
  return {
    result: failingTiers.includes(worst.tier) ? 'failure' : 'loading',
    message: summary,
  }
}

/** Fetch the alert list from the daemon, or null if it is not answering yet. */
async function readAlerts(
  container: Container,
): Promise<ForktowerAlert[] | null> {
  const res = await container
    .exec(
      [
        'curl',
        '-fsS',
        '--max-time',
        '10',
        `http://127.0.0.1:${dashboardPort}/api/v1/alerts?unacked=true`,
      ],
    )
    .catch(() => null)

  if (!res || res.exitCode !== 0) return null

  try {
    const body = JSON.parse(res.stdout.toString())
    const data = body?.data
    return Array.isArray(data) ? (data as ForktowerAlert[]) : []
  } catch {
    return null
  }
}

/** Announce whatever has not been announced before, and remember that it was. */
async function raiseUnannounced(
  effects: Effects,
  standing: ForktowerAlert[],
): Promise<void> {
  const store = await storeJson
    .read()
    .const(effects as never)
    .catch(() => null)
  const announced = new Set(store?.announcedAlerts ?? [])

  const fresh = standing.filter((a) => !announced.has(a.dedup_key))
  if (fresh.length === 0) return

  for (const alert of fresh) {
    const level = notificationLevels[alert.tier as AlertTier] ?? 'info'
    await effects.notification
      .create({
        level,
        title: alert.subject,
        // The panel row is short; the explanation is long and is worth reading
        // in full, so it goes in `data` where the platform renders it behind
        // "View Details" as markdown. Before this existed the choice was to
        // truncate it or drop it, and both lose the half that says what to do.
        message: firstSentence(alert.message),
        data: alert.message,
      })
      .catch(() => null)
    announced.add(alert.dedup_key)
  }

  // Bounded, oldest first. See `announcedAlertsKept` for why there is a limit
  // at all.
  const kept = Array.from(announced).slice(-announcedAlertsKept)
  await storeJson.merge(effects as never, { announcedAlerts: kept }).catch(
    () => null,
  )
}

/**
 * The first sentence, for the notification's one-line body.
 *
 * Falls back to the whole message when there is no sentence break — a short
 * message is already a summary, and cutting it at an arbitrary column would
 * only make it look broken.
 */
function firstSentence(message: string): string {
  const end = message.search(/[.!?](\s|$)/)
  if (end === -1 || end > 200) {
    return message.length > 200 ? `${message.slice(0, 197)}...` : message
  }
  return message.slice(0, end + 1)
}
