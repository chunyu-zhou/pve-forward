<template>
  <a-layout class="layout">
    <div class="layout-navbar">
      <div class="navbar">
        <div class="left-side">
          <a-space>
            <div class="logo-mark"><icon-swap /></div>
            <a-typography-title :style="{ margin: 0, fontSize: '18px' }" :heading="5">{{ session.site }}</a-typography-title>
          </a-space>
        </div>
        <ul class="right-side">
          <li v-if="admin && session.updateAvailable">
            <a-tag color="arcoblue" style="cursor:pointer" @click="$router.push('/settings')">
              <template #icon><icon-cloud-download /></template>新版本 v{{ session.updateAvailable }}</a-tag>
          </li>
          <li v-if="admin && nodeSum.total">
            <a-tag :color="nodeSum.online === nodeSum.ready ? 'green' : 'orange'" style="cursor:pointer" @click="$router.push('/nodes')">
              <template #icon><icon-cloud /></template>节点 {{ nodeSum.online }} / {{ nodeSum.total }} 在线</a-tag>
          </li>
          <li>
            <a-tooltip content="全屏"><a-button class="nav-btn" type="outline" shape="circle" @click="toggleFull"><template #icon><icon-fullscreen /></template></a-button></a-tooltip>
          </li>
          <li>
            <a-dropdown trigger="click">
              <a-avatar :size="32" :style="{ cursor: 'pointer', backgroundColor: admin ? '#165dff' : '#00b42a' }">{{ (session.user?.username || '?')[0].toUpperCase() }}</a-avatar>
              <template #content>
                <a-doption disabled><a-space><icon-user /><span>{{ session.user?.username }}（{{ admin ? '管理员' : '用户' }}）</span></a-space></a-doption>
                <a-doption @click="$router.push('/profile')"><a-space><icon-settings /><span>个人资料</span></a-space></a-doption>
                <a-doption @click="logout"><a-space><icon-export /><span>退出登录</span></a-space></a-doption>
              </template>
            </a-dropdown>
          </li>
        </ul>
      </div>
    </div>
    <a-layout>
      <a-layout-sider class="layout-sider" breakpoint="xl" :collapsed="collapsed" collapsible :width="220" hide-trigger @collapse="v => (collapsed = v)">
        <div class="menu-wrapper">
          <a-menu v-model:collapsed="collapsed" show-collapse-button :auto-open="false" :level-indent="34"
                  :selected-keys="[menuKey]" v-model:open-keys="openKeys" style="height:100%;width:100%" @menu-item-click="k => $router.push(k)">
            <a-menu-item key="/dashboard"><template #icon><icon-dashboard /></template>仪表盘</a-menu-item>
            <a-menu-item key="/monitor"><template #icon><icon-desktop /></template>资源监控</a-menu-item>
            <a-sub-menu key="fwd"><template #icon><icon-swap /></template><template #title>转发管理</template>
              <a-menu-item key="/rules">转发规则</a-menu-item>
            </a-sub-menu>
            <a-sub-menu v-if="admin" key="user"><template #icon><icon-user-group /></template><template #title>用户管理</template>
              <a-menu-item key="/users">用户列表</a-menu-item>
              <a-menu-item key="/users/new">添加用户</a-menu-item>
            </a-sub-menu>
            <a-sub-menu v-if="admin" key="pve"><template #icon><icon-cloud /></template><template #title>PVE 节点</template>
              <a-menu-item key="/nodes">节点列表</a-menu-item>
              <a-menu-item key="/nodes/new">添加节点</a-menu-item>
              <a-menu-item key="/vms">虚拟机</a-menu-item>
              <a-menu-item key="/credentials">SSH 凭证</a-menu-item>
            </a-sub-menu>
            <a-menu-item v-if="admin" key="/settings"><template #icon><icon-settings /></template>系统设置</a-menu-item>
            <a-sub-menu key="me"><template #icon><icon-user /></template><template #title>个人中心</template>
              <a-menu-item key="/profile">个人资料</a-menu-item>
            </a-sub-menu>
          </a-menu>
        </div>
      </a-layout-sider>
      <a-layout class="layout-content" :style="{ paddingLeft: (collapsed ? 48 : 220) + 'px' }">
        <a-layout-content>
          <div v-if="$route.meta.title && $route.path !== '/dashboard'" class="container" style="padding-bottom:0">
            <a-breadcrumb class="container-breadcrumb" style="margin-bottom:0">
              <a-breadcrumb-item><icon-apps /></a-breadcrumb-item>
              <a-breadcrumb-item v-for="t in $route.meta.title" :key="t">{{ t }}</a-breadcrumb-item>
            </a-breadcrumb>
          </div>
          <router-view v-slot="{ Component }">
            <component :is="Component" :key="$route.fullPath" />
          </router-view>
        </a-layout-content>
      </a-layout>
    </a-layout>
  </a-layout>
</template>

<script setup>
import { computed, onMounted, onUnmounted, provide, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, session, isAdmin } from '../api'

const route = useRoute()
const router = useRouter()
const collapsed = ref(false)
provide('collapsed', collapsed)
const admin = computed(isAdmin)
const menuKey = computed(() => route.meta.menu || route.path)
const parentOf = { '/rules': 'fwd', '/users': 'user', '/users/new': 'user', '/nodes': 'pve', '/nodes/new': 'pve', '/vms': 'pve', '/credentials': 'pve', '/profile': 'me' }
const openKeys = ref([parentOf[menuKey.value] || 'fwd'])
watch(menuKey, k => { const p = parentOf[k]; if (p && !openKeys.value.includes(p)) openKeys.value = [...openKeys.value, p] })

const nodeSum = ref({ total: 0, online: 0, ready: 0 })
async function loadNodes() {
  if (!admin.value) return
  try {
    const d = await api.get('/api/nodes', { silent: true })
    nodeSum.value = { total: d.nodes.length, online: d.nodes.filter(n => n.status === 'online').length, ready: d.nodes.filter(n => n.initialized).length }
  } catch { /* ignore */ }
}
let timer
onMounted(() => { loadNodes(); timer = setInterval(loadNodes, 30000) })
onUnmounted(() => clearInterval(timer))

async function logout() {
  await api.post('/api/logout')
  session.user = null
  router.push('/login')
}
function toggleFull() {
  if (document.fullscreenElement) document.exitFullscreen()
  else document.documentElement.requestFullscreen()
}
</script>

<style scoped>
.layout { width: 100%; height: 100%; }
.layout-navbar { position: fixed; top: 0; left: 0; z-index: 100; width: 100%; height: 60px; }
.layout-sider { position: fixed !important; top: 0; left: 0; z-index: 99; height: 100%; padding-top: 60px; transition: all .2s cubic-bezier(.34,.69,.1,1); }
.layout-sider::after { position: absolute; top: 0; right: -1px; display: block; width: 1px; height: 100%; background: var(--color-border); content: ''; }
.layout-sider :deep(.arco-layout-sider-children) { overflow-y: hidden; }
.menu-wrapper { height: 100%; overflow: auto; overflow-x: hidden; }
.menu-wrapper :deep(.arco-menu-inner .arco-menu-inline-header) { display: flex; align-items: center; }
.menu-wrapper :deep(.arco-menu-inner .arco-icon:not(.arco-icon-down)) { font-size: 18px; }
.layout-content { min-height: 100vh; background-color: var(--color-fill-2); transition: padding .2s cubic-bezier(.34,.69,.1,1); padding-top: 60px; }
.navbar { display: flex; justify-content: space-between; height: 100%; background: var(--color-bg-2); border-bottom: 1px solid var(--color-border); }
.left-side { display: flex; align-items: center; padding-left: 20px; }
.right-side { display: flex; padding-right: 20px; list-style: none; margin: 0; }
.right-side li { display: flex; align-items: center; padding: 0 10px; }
.nav-btn { border-color: rgb(var(--gray-2)); color: rgb(var(--gray-8)); font-size: 16px; }
</style>
