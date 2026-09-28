export function formatUpgradeStage(stage?: string): string {
  switch (stage) {
    case 'dispatched':
      return '已下发'
    case 'downloading':
      return '下载中'
    case 'verifying':
      return '校验完整性'
    case 'replacing':
      return '替换程序'
    case 'restarting':
      return '平滑重启'
    case 'error':
      return '升级失败'
    default:
      return '升级中'
  }
}
