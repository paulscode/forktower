import { configJson } from '../fileModels/config.json'
import { storeJson } from '../fileModels/store.json'
import { sdk } from '../sdk'

/**
 * Runs once on install, and again after every version migration.
 *
 * Writes both files so that everything downstream can read them rather than
 * guess. There are no secrets to generate here — the second Bitcoin node's RPC
 * password is made by the entrypoint inside the container, where it is used, and
 * Forktower holds no credentials of its own.
 *
 * Every field is defaulted by the schema, so `merge({})` writes a complete file
 * without restating the defaults in a second place that could disagree with the
 * first.
 */
export const seedFiles = sdk.setupOnInit(async (effects) => {
  await configJson.merge(effects, {})
  await storeJson.merge(effects, {})
})
