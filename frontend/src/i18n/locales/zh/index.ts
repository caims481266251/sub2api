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
    manage: '补充或修改已保存信息',
    loading: '正在读取 2FA 授权状态…', saved: '默认使用已加密保存的 2FA 信息重新授权。',
    missing: '尚未保存完整 2FA 信息，请先在凭证运营中补全。', reauthorize: '使用已保存信息重新授权',
    copy: '复制 2FA 完整信息', copied: '已复制到剪贴板', refresh: '刷新状态',
    loadFailed: '授权状态读取失败，请重试。', startFailed: '重新授权未启动，请检查凭证运营中的登录服务后重试。',
    copyFailed: '复制失败，请检查已保存信息及浏览器剪贴板权限。',
    states: { queued: '等待重新授权', running: '正在重新授权', callback_processing: '正在保存授权', succeeded: '重新授权成功', failed: '重新授权失败，请检查凭据或代理后手动重试' }
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
