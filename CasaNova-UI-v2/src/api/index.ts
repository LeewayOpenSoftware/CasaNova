// Barrel re-export for the API client layer. Components and stores
// import from '@/api' so adding a new endpoint module is a one-line
// change and import sites stay short.
//
// Keep this file alphabetised by module name; grouping by concern
// turns into bikeshedding over what belongs where.

export { default as client } from './client'
export { safeCall, isReachable, type SafeCallResult } from './safeCall'

// Per-domain modules. Each file is a thin typed wrapper around the
// matching CasaNova service's routes.
export * as alertsApi from './alerts'
export * as cronApi from './cron'
export * as containersApi from './containers'
export * as filesApi from './files'
export * as libraryApi from './library'
export * as hardwareApi from './hardware'
export * as hostApi from './host'
export * as maintenanceApi from './maintenance'
export * as migrationApi from './migration'
export * as notificationsApi from './notifications'
export * as remoteAccessApi from './remoteAccess'
export * as storageApi from './storage'
export * as systemApi from './system'
export * as themePacksApi from './themePacks'
export * as updatesApi from './updates'
export * as usersApi from './users'
export * as yskApi from './ysk'
