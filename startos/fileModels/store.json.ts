import { FileHelper, z } from '@start9labs/start-sdk'
import { sdk } from '../sdk'

/**
 * What the wrapper remembers between passes.
 *
 * Only one thing so far: which alerts have already been announced to the
 * platform's notification centre. It has to survive a restart — otherwise every
 * update re-announces every standing alert, and a user who has just upgraded is
 * met with a screen of notifications about things they already knew and had
 * already acted on. That teaches people to ignore the notifications, which
 * costs more than the ones it delivered.
 *
 * Keyed by the alert's `dedup_key` rather than its row id, because the id is a
 * database detail and the dedup key is the identity the daemon itself uses to
 * decide whether two alerts are the same thing said twice.
 */
export const storeJson = FileHelper.json(
  {
    base: sdk.volumes.main,
    subpath: '/store.json',
  },
  z.object({
    announcedAlerts: z.array(z.string()).catch([]),
  }),
)

/**
 * How many announcement keys to keep.
 *
 * Bounded because this file is written on every new alert and read on every
 * pass, and an unbounded list of every alert ever raised would grow for the life
 * of the installation to save re-announcing something from two years ago. The
 * oldest keys are dropped first; the cost of being wrong is one duplicate
 * notification about an alert that has been standing for a very long time.
 */
export const announcedAlertsKept = 500
