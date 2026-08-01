import { sdk } from './sdk'

/**
 * What is worth keeping.
 *
 * The daemon's database and settings, and **not** the second Bitcoin node's
 * datadir. That exclusion is the whole point of writing this by hand rather than
 * backing up the volume whole: the chain is tens of gigabytes that every full
 * node on earth already has, and putting it in the user's backup would make the
 * backup enormous, slow, and no more useful — a restored node re-syncs either
 * way.
 *
 * What is lost by excluding it is sync time after a restore. What is gained is a
 * backup a person will actually take.
 */
export const { createBackup, restoreInit } = sdk.setupBackups(async () =>
  sdk.Backups.withOptions({
    exclude: ['sq/'],
  }).addVolume('main'),
)
