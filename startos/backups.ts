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
 *
 * The watchtower splits the same way, and the split is not obvious.
 *
 * Its **wallet is kept**, because the tower's identity key is derived from it.
 * That key is half of the address the user pasted into their own node; lose it
 * and the registration they made silently points at a tower that no longer
 * answers to that name, with nothing anywhere saying so.
 *
 * Its **blob store is dropped**. It holds encrypted backups belonging to the
 * node sitting next to it, is capped at two gigabytes, and would undo the
 * argument above about a backup somebody will actually take. The cost is real
 * and worth stating: after a restore the tower cannot answer for states it was
 * given before the backup, and the client will not re-send them. Coverage
 * resumes from the next state update.
 */
export const { createBackup, restoreInit } = sdk.setupBackups(async () =>
  sdk.Backups.withOptions({
    exclude: ['sq/', 'tower/towerdata/'],
  }).addVolume('main'),
)
