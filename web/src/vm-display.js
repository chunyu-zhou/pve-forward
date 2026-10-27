export function userLabel(username, remark) {
  const name = typeof username === 'string' ? username.trim() : ''
  const note = typeof remark === 'string' ? remark.trim() : ''
  return note ? `${name} (${note})` : name
}

export function vmName(vm) {
  const name = typeof vm.name === 'string' ? vm.name.trim() : ''
  return name || `未命名 ${vm.type === 'lxc' ? 'CT' : 'VM'} ${vm.vmid}`
}

export function vmState(vm) {
  if (vm.present === false || vm.status === 'deleted') {
    return { label: '已删除', className: 'err', tooltip: '' }
  }

  const nodeReasons = {
    offline: '节点离线',
    connecting: '节点连接中',
    unknown: '节点状态未知',
    waiting: '节点等待连接或首次采集',
    pending: '节点等待初始化',
  }
  const reason = (Object.hasOwn(nodeReasons, vm.nodeStatus) ? nodeReasons[vm.nodeStatus] : '') ||
    (vm.status === 'offline' ? '节点离线' : '') ||
    (vm.nodeStale === true ? '节点数据已过期' : '')
  if (reason) {
    return { label: '待确认', className: 'warn', tooltip: `${reason}，当前显示旧缓存，虚拟机状态待确认` }
  }

  switch (vm.status) {
    case 'running':
      return { label: '运行中', className: 'pass', tooltip: '' }
    case 'stopped':
      return { label: '已停止', className: '', tooltip: '' }
    case 'paused':
    case 'suspended':
      return { label: '已暂停', className: 'warn', tooltip: '' }
    default:
      return { label: '状态未知', className: 'warn', tooltip: '' }
  }
}
