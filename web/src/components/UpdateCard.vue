<template>
  <a-card class="general-card" title="版本更新">
    <template #extra>
      <a-button size="small" :loading="checking" :disabled="job.running" @click="check"><template #icon><icon-refresh /></template>检测更新</a-button>
    </template>
    <a-spin :loading="!info" style="width:100%">
      <template v-if="info">
        <a-descriptions :column="{ xs: 1, md: 3 }" :data="[
          { label: '当前版本', value: 'v' + info.current },
          { label: '最新版本', value: info.latest ? 'v' + info.latest.version + (info.latest.date ? `（${info.latest.date}）` : '') : '未检测' },
          { label: '上次检测', value: info.lastCheck ? fmtTime(info.lastCheck) : '尚未检测' } ]" />

        <a-alert v-if="info.error" type="warning" style="margin-top:12px">检测失败：{{ info.error }}。可在下方配置 GitHub 下载加速前缀，或为容器设置 HTTPS_PROXY。</a-alert>
        <a-alert v-if="info.failed" type="error" style="margin-top:12px">v{{ info.failed }} 未能正常启动，已自动回退到当前版本。请查看控制端日志后再尝试。</a-alert>

        <template v-if="job.running || job.error">
          <div style="margin-top:16px">
            <div class="muted" style="margin-bottom:6px">{{ job.running ? `正在更新到 v${job.target}：${job.step}` : job.step }}</div>
            <a-progress :percent="job.percent / 100" :status="job.error ? 'danger' : job.percent >= 100 ? 'success' : 'normal'" />
            <a-alert v-if="job.error" type="error" style="margin-top:8px">{{ job.error }}</a-alert>
            <div v-if="restarting" class="muted" style="margin-top:6px">控制端正在重启，完成后页面会自动刷新…</div>
          </div>
        </template>

        <template v-else-if="info.available">
          <a-alert type="success" style="margin-top:12px">
            <template #title>发现新版本 v{{ info.available }}</template>
            <div v-if="info.latest?.notes" class="update-notes">{{ info.latest.notes }}</div>
          </a-alert>
          <a-row :gutter="16" style="margin-top:16px">
            <a-col :xs="24" :md="12">
              <div class="update-way">
                <div class="update-way-title">在线更新程序</div>
                <div class="muted">从 GitHub Release 下载 v{{ info.available }} 并校验 SHA256，备份数据库后自动重启控制端。节点规则不受影响，约 1 分钟内恢复访问；新版本启动失败会自动回退。</div>
                <a-popconfirm content="更新期间控制端会短暂重启，确定更新？" @ok="apply">
                  <a-button type="primary" style="margin-top:12px" :disabled="!info.canUpdate"><template #icon><icon-cloud-download /></template>立即更新到 v{{ info.available }}</a-button>
                </a-popconfirm>
                <div v-if="!info.canUpdate" class="muted" style="margin-top:6px">{{ info.reason }}</div>
              </div>
            </a-col>
            <a-col :xs="24" :md="12">
              <div class="update-way">
                <div class="update-way-title">更新 Docker 镜像</div>
                <div class="muted">在部署目录执行以下命令拉取新镜像并重建容器；数据卷保留，账号与配置不变。</div>
                <div class="cmd" style="margin-top:12px">{{ dockerCmd }}<a-button class="copy" size="mini" @click="copyText(dockerCmd)"><template #icon><icon-copy /></template>复制</a-button></div>
              </div>
            </a-col>
          </a-row>
          <div v-if="info.runtime === 'docker'" class="muted" style="margin-top:8px">
            当前运行在 Docker 中：在线更新的程序保存在数据卷中，重建容器后仍会运行；之后拉取的镜像版本不低于它时自动改用镜像内的程序。
          </div>
        </template>

        <a-result v-else-if="info.latest && !info.available" status="success" title="已是最新版本" style="padding:16px 0 0">
          <template #subtitle>
            <span v-if="info.latest && cmp(info.latest.version, info.current) > 0 && !info.ready">v{{ info.latest.version }} 正在 GitHub 构建，发布后即可更新</span>
            <span v-else>当前 v{{ info.current }}</span>
          </template>
        </a-result>

        <a-divider style="margin:20px 0 16px" />
        <a-row :gutter="80">
          <a-col :xs="24" :md="8"><a-form-item label="自动检测新版本" extra="每小时读取 GitHub 仓库中的 version.json，只提示不自动安装">
            <a-switch v-model="st.updateCheck" /></a-form-item></a-col>
          <a-col :xs="24" :md="16"><a-form-item label="GitHub 下载加速前缀（选填）" extra="无法直接访问 GitHub 时填写 https 地址，如 https://ghproxy.example.com/ ，会拼接在 GitHub 地址之前；点击“检测更新”时生效。校验文件优先直连 GitHub 获取，请只使用可信的加速服务">
            <a-input v-model="st.updateMirror" placeholder="留空则直接访问 GitHub" allow-clear /></a-form-item></a-col>
        </a-row>
      </template>
    </a-spin>
  </a-card>
</template>

<script setup>
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { Message } from '@arco-design/web-vue'
import { api, copyText, fmtTime, loadSession, session } from '../api'

const props = defineProps({ st: { type: Object, required: true } })

const info = ref(null)
const checking = ref(false)
const restarting = ref(false)
const job = computed(() => info.value?.job || {})
const dockerCmd = 'docker compose pull && docker compose up -d'
let timer = null

// 与后端一致的版本比较（主.次.修订，预发布低于正式版）
function cmp(a, b) {
  const p = v => { const [m, pre = ''] = String(v).split('-'); return [m.split('.').map(Number), pre] }
  const [x, xp] = p(a), [y, yp] = p(b)
  for (let i = 0; i < 3; i++) if (x[i] !== y[i]) return x[i] > y[i] ? 1 : -1
  if (xp === yp) return 0
  return !xp ? 1 : !yp ? -1 : xp > yp ? 1 : -1
}

async function load() { info.value = await api.get('/api/update') }

async function check() {
  checking.value = true
  try {
    info.value = await api.post('/api/update/check', { mirror: props.st.updateMirror || '' })
    if (info.value.error) Message.warning('检测失败：' + info.value.error)
    else if (info.value.available) Message.success(`发现新版本 v${info.value.available}`)
    else Message.info('已是最新版本')
    session.updateAvailable = info.value.available || ''
  } finally { checking.value = false }
}

async function apply() {
  info.value = await api.post('/api/update/apply', { version: info.value.available })
  poll()
}

// 跟踪下载进度；控制端重启时接口会短暂不可用，待新版本上线后刷新页面
function poll() {
  clearTimeout(timer)
  const target = info.value?.job?.target
  const started = Date.now()
  const tick = async () => {
    try {
      if (restarting.value) {
        const b = await api.get('/api/bootstrap', { silent: true })
        if (b.version === target) { Message.success(`已更新到 v${target}`); setTimeout(() => location.reload(), 800); return }
        // 控制端已恢复但不是新版本：新版本启动失败并已回退
        const u = await api.get('/api/update', { silent: true })
        if (u.failed === target) { info.value = u; restarting.value = false; Message.error(`v${target} 启动失败，已回退到 v${u.current}`); return }
      } else {
        info.value = await api.get('/api/update', { silent: true })
        if (!info.value.job.running) return
        if (info.value.job.percent >= 100) restarting.value = true
      }
    } catch { restarting.value = true }
    if (Date.now() - started > 15 * 60 * 1000) { Message.error('等待控制端重启超时，请检查控制端日志'); return }
    timer = setTimeout(tick, restarting.value ? 2000 : 1000)
  }
  tick()
}

onMounted(async () => { await load(); if (info.value.job?.running) poll() })
onUnmounted(() => clearTimeout(timer))
defineExpose({ reload: async () => { await loadSession(); await load() } })
</script>
