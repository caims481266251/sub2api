import ipAudit from './ipAudit'
import connections from './connections'
import securityRecords from './securityRecords'
import siwc from './siwc'
import priorityScheduling from './priorityScheduling'
import qualityOps from './qualityOps'
import controlledExperiments from './controlledExperiments'
import accountOps from './accountOps'
import tokenGuard from './tokenGuard'
import pelicanTests from './pelicanTests'
import modelTrace from './modelTrace'
import tokenGuardV2 from './tokenGuardV2'
import landing from './landing'
import common from './common'
import dashboard from './dashboard'
import channelMonitorV2 from './channelMonitorV2'
import channelMonitorV3 from './channelMonitorV3'
import supportTickets from './supportTickets'
import batchImage from './batchImage'
import admin from './admin'
import misc from './misc'

import requestTiming from './requestTiming'

import autoConfig from './autoConfig'

export default {
  savedTwoFA: {
    manage: 'Add or edit saved login',
    loading: 'Loading 2FA authorization status…', saved: 'Reauthorize using the encrypted login information saved for this account.',
    missing: 'Complete the saved 2FA login in Credential Operations first.', reauthorize: 'Reauthorize with saved login',
    copy: 'Copy complete 2FA login', copied: 'Copied to clipboard', refresh: 'Refresh status',
    loadFailed: 'Could not load authorization status. Try again.', startFailed: 'Could not start reauthorization. Check the login service in Credential Operations.',
    copyFailed: 'Could not copy. Check the saved login and browser clipboard permission.',
    states: { queued: 'Waiting to reauthorize', running: 'Reauthorizing', callback_processing: 'Saving authorization', succeeded: 'Reauthorization succeeded', failed: 'Reauthorization failed. Check credentials or proxy and retry manually.' }
  },

  ipAudit,
  connections,
  securityRecords,
  siwc,
  autoConfig,
  priorityScheduling,
  qualityOps,
  controlledExperiments,
  accountOps,
  tokenGuard,
  pelicanTests,
  modelTrace,
  tokenGuardV2,
  requestTiming,
  ...landing,
  ...common,
  ...dashboard,
  ...channelMonitorV2,
  ...channelMonitorV3,
  ...supportTickets,
  ...batchImage,
  admin,
  ...misc,
}
