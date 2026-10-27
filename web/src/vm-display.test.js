import test from 'node:test'
import assert from 'node:assert/strict'
import { vmName, vmState, userLabel } from './vm-display.js'

test('归属用户名追加备注，无备注不加括号', () => {
  assert.equal(userLabel('zhangsan', '张三'), 'zhangsan (张三)')
  assert.equal(userLabel(' zhangsan ', ' 客户A '), 'zhangsan (客户A)')
  for (const remark of ['', '  ', null, undefined]) {
    assert.equal(userLabel('zhangsan', remark), 'zhangsan')
  }
})

test('名称保留有效内容并去掉首尾空白', () => {
  assert.equal(vmName({ name: ' database ', vmid: 100, type: 'qemu' }), 'database')
  assert.equal(vmName({ name: '容器', vmid: 101, type: 'lxc' }), '容器')
})

test('空名称按类型和 VMID 生成名称', () => {
  for (const name of [undefined, null, '', '  ', '\t\n', '\u3000']) {
    assert.equal(vmName({ name, vmid: 100, type: 'qemu' }), '未命名 VM 100')
    assert.equal(vmName({ name, vmid: 101, type: 'lxc' }), '未命名 CT 101')
  }
})

test('兼容没有 nodeStatus 字段的旧数据', () => {
  for (const [status, label, className] of [
    ['running', '运行中', 'pass'],
    ['stopped', '已停止', ''],
    ['paused', '已暂停', 'warn'],
    ['suspended', '已暂停', 'warn'],
  ]) {
    assert.deepEqual(vmState({ status }), { label, className, tooltip: '' })
    assert.deepEqual(vmState({ present: true, nodeStatus: 'online', status }), { label, className, tooltip: '' })
  }
})

test('未知或空状态不会显示为已停止', () => {
  for (const status of [undefined, null, '', 'unknown', 'starting', 'error']) {
    assert.deepEqual(vmState({ status }), { label: '状态未知', className: 'warn', tooltip: '' })
  }
})

test('不可确认的节点状态覆盖虚拟机缓存状态并解释旧缓存原因', () => {
  for (const [nodeStatus, reason] of [
    ['offline', '节点离线'],
    ['connecting', '节点连接中'],
    ['unknown', '节点状态未知'],
    ['waiting', '节点等待连接或首次采集'],
    ['pending', '节点等待初始化'],
  ]) {
    for (const status of ['running', 'stopped', 'paused', undefined]) {
      const state = vmState({ present: true, nodeStatus, status })
      assert.equal(state.label, '待确认')
      assert.equal(state.className, 'warn')
      assert.ok(state.tooltip.includes(reason))
      assert.ok(state.tooltip.includes('旧缓存'))
    }
  }
})

test('旧版 offline 状态显示待确认', () => {
  const state = vmState({ status: 'offline' })
  assert.equal(state.label, '待确认')
  assert.equal(state.className, 'warn')
  assert.match(state.tooltip, /节点离线.*旧缓存/)
})

test('nodeStale 标记使在线或无节点状态的数据待确认', () => {
  for (const nodeStatus of ['online', undefined]) {
    for (const status of ['running', 'stopped']) {
      const state = vmState({ nodeStatus, nodeStale: true, status })
      assert.equal(state.label, '待确认')
      assert.equal(state.className, 'warn')
      assert.match(state.tooltip, /数据已过期.*旧缓存/)
    }
  }
  assert.equal(vmState({ status: 'running', nodeStale: false }).label, '运行中')
})

test('已删除优先于节点离线和缓存状态', () => {
  for (const vm of [
    { present: false, status: 'running' },
    { status: 'deleted' },
    { present: true, status: 'deleted', nodeStatus: 'offline', nodeStale: true },
    { present: false, status: 'stopped', nodeStatus: 'connecting', nodeStale: true },
  ]) {
    assert.deepEqual(vmState(vm), { label: '已删除', className: 'err', tooltip: '' })
  }
})
