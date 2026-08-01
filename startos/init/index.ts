import { actions } from '../actions'
import { restoreInit } from '../backups'
import { setDependencies } from '../dependencies'
import { setInterfaces } from '../interfaces'
import { sdk } from '../sdk'
import { versionGraph } from '../versions'
import { seedFiles } from './seedFiles'

/**
 * What happens when the package is installed, updated or restored.
 *
 * This is where the interfaces get exported and the dependencies get declared —
 * they are not wired by being defined. A package that exports its `main` but not
 * its `init` builds and packs, and then has no dashboard address and no
 * dependency on anything.
 */
export const init = sdk.setupInit(
  restoreInit,
  versionGraph,
  seedFiles,
  setInterfaces,
  setDependencies,
  actions,
)

export const uninit = sdk.setupUninit(versionGraph)
