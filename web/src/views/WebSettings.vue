<template>
  <div class="container" style="padding-top:16px">
    <a-card class="general-card">
      <a-tabs v-model:active-key="tab" @change="k => $router.replace({ query: { tab: k } })">
        <!-- ================= 主域名 ================= -->
        <a-tab-pane key="domains" title="主域名">
          <a-row justify="space-between" align="center" style="margin-bottom:16px">
            <span class="muted">用户只能使用这里的主域名，填写前缀即可（如 dev-zcy + .example.com）。保存站点后，系统通过所选 DNS 服务商自动添加解析，指向节点公网 IP</span>
            <a-button type="primary" :disabled="!providers.length" @click="openDomain()"><template #icon><icon-plus /></template>添加主域名</a-button>
          </a-row>
          <a-alert v-if="!providers.length" style="margin-bottom:16px">请先在「DNS 服务商」中添加服务商账号，再添加主域名</a-alert>
          <a-table :data="domains" :pagination="false" :bordered="false" row-key="id" :loading="loading && !loaded">
            <template #columns>
              <a-table-column title="主域名" :width="200"><template #cell="{ record }"><b class="mono" style="font-size:13px">{{ record.name }}</b></template></a-table-column>
              <a-table-column title="DNS 服务商" :width="220"><template #cell="{ record }"><div class="lines"><div>{{ record.providerName }}</div><div class="muted">{{ record.providerType }}</div></div></template></a-table-column>
              <a-table-column title="可用用户" :width="180"><template #cell="{ record }">{{ record.users.length ? record.userNames.join('、') : '全部用户' }}</template></a-table-column>
              <a-table-column title="可用证书" :width="220"><template #cell="{ record }"><span v-if="record.certs.length">{{ record.certs.join('、') }}</span><span v-else class="muted">无（只能用 HTTP 入口）</span></template></a-table-column>
              <a-table-column title="已用" :width="100"><template #cell="{ record }">{{ record.sites }} 个站点</template></a-table-column>
              <a-table-column title="操作" :width="130"><template #cell="{ record }"><a-space :size="0">
                <a-button type="text" size="small" @click="openDomain(record)">编辑</a-button>
                <a-popconfirm content="确定删除该主域名？" type="warning" @ok="del(`/api/web-domains/${record.id}`)"><a-button type="text" size="small" status="danger" :disabled="record.sites > 0">删除</a-button></a-popconfirm>
              </a-space></template></a-table-column>
            </template>
            <template #empty><a-empty description="还没有主域名" /></template>
          </a-table>
        </a-tab-pane>

        <!-- ================= DNS 服务商 ================= -->
        <a-tab-pane key="dns" title="DNS 服务商">
          <a-row justify="space-between" align="center" style="margin-bottom:16px">
            <span class="muted">同一类服务商可添加多个账号；密钥加密保存，保存后不再显示</span>
            <a-button type="primary" @click="openProvider()"><template #icon><icon-plus /></template>添加 DNS 服务商</a-button>
          </a-row>
          <a-table :data="providers" :pagination="false" :bordered="false" row-key="id" :loading="loading && !loaded">
            <template #columns>
              <a-table-column title="名称" :width="200" data-index="name" />
              <a-table-column title="类型" :width="140" data-index="typeName" />
              <a-table-column title="账号" :width="240"><template #cell="{ record }"><span class="mono">{{ record.account }}</span></template></a-table-column>
              <a-table-column title="状态" :width="200"><template #cell="{ record }">
                <span v-if="!record.status"><span class="circle pass"></span>验证通过</span>
                <a-tooltip v-else :content="record.status"><span style="color:rgb(var(--red-6));cursor:help"><span class="circle err"></span>验证失败 <icon-question-circle /></span></a-tooltip>
              </template></a-table-column>
              <a-table-column title="使用的主域名"><template #cell="{ record }">{{ record.domains.join('、') || '-' }}</template></a-table-column>
              <a-table-column title="操作" :width="180"><template #cell="{ record }"><a-space :size="0">
                <a-button type="text" size="small" :loading="testing === record.id" @click="testSaved(record)">测试</a-button>
                <a-button type="text" size="small" @click="openProvider(record)">编辑</a-button>
                <a-popconfirm content="确定删除该 DNS 服务商？" type="warning" @ok="del(`/api/dns-providers/${record.id}`)"><a-button type="text" size="small" status="danger" :disabled="record.domains.length > 0">删除</a-button></a-popconfirm>
              </a-space></template></a-table-column>
            </template>
            <template #empty><a-empty description="还没有 DNS 服务商" /></template>
          </a-table>
        </a-tab-pane>

        <!-- ================= 证书 ================= -->
        <a-tab-pane key="certs" title="证书">
          <a-row justify="space-between" align="center" style="margin-bottom:16px">
            <span class="muted">由管理员手动添加；用户添加 HTTPS 站点时，只能选择覆盖其域名的证书。证书到期前 30 天、7 天会在事件通知中提醒</span>
            <a-button type="primary" @click="openCert()"><template #icon><icon-plus /></template>添加证书</a-button>
          </a-row>
          <a-table :data="certs" :pagination="false" :bordered="false" row-key="id" :loading="loading && !loaded">
            <template #columns>
              <a-table-column title="名称" :width="180" data-index="name" />
              <a-table-column title="包含域名" :width="260"><template #cell="{ record }"><div class="lines mono"><div v-for="d in record.domains" :key="d">{{ d }}</div></div></template></a-table-column>
              <a-table-column title="颁发者" :width="180" data-index="issuer" />
              <a-table-column title="到期时间" :width="170"><template #cell="{ record }"><div class="lines"><div>{{ fmtDate(record.notAfter) }}</div>
                <div class="muted" :style="{ color: expColor(record.days) }">{{ record.days < 0 ? '已过期' : `${record.days} 天后到期` }}</div></div></template></a-table-column>
              <a-table-column title="使用站点" :width="100"><template #cell="{ record }">{{ record.sites }} 个</template></a-table-column>
              <a-table-column title="操作" :width="130"><template #cell="{ record }"><a-space :size="0">
                <a-button type="text" size="small" @click="openCert(record)">编辑</a-button>
                <a-popconfirm content="确定删除该证书？" type="warning" @ok="del(`/api/certs/${record.id}`)"><a-button type="text" size="small" status="danger" :disabled="record.sites > 0">删除</a-button></a-popconfirm>
              </a-space></template></a-table-column>
            </template>
            <template #empty><a-empty description="还没有证书" /></template>
          </a-table>
        </a-tab-pane>

        <!-- ================= Web 入口 ================= -->
        <a-tab-pane key="entry" title="Web 入口">
          <a-space direction="vertical" :size="16" fill>
            <a-empty v-if="!entries.length" description="还没有节点" />
            <div v-for="e in entries" :key="e.node.id" class="entry-card">
              <a-row justify="space-between" align="center" style="margin-bottom:16px">
                <a-space wrap>
                  <b>{{ e.node.name }}</b><span class="muted mono">{{ e.node.wanIp4 }}</span>
                  <a-badge v-if="e.state.location && e.report && !e.report.up" status="danger" :text="e.report.error || 'OpenResty 未运行'" />
                  <a-badge v-else-if="e.state.location" status="success" :text="`OpenResty 运行中 · ${shortVersion(e.state.version)} · ${locName(e, e.state.location)}`" />
                  <a-tag v-if="e.state.error" color="red" size="small">配置下发失败</a-tag>
                </a-space>
                <a-space><span class="muted">启用 Web 转发</span><a-switch v-model="forms[e.node.id].enabled" :disabled="!e.node.initialized" /></a-space>
              </a-row>
              <a-alert v-if="e.state.error" type="error" style="margin-bottom:16px" title="最近一次配置下发失败（站点保持原配置运行）">{{ e.state.error }}</a-alert>
              <template v-if="forms[e.node.id].enabled">
                <a-form :model="forms[e.node.id]" layout="vertical">
                  <a-form-item label="部署方式" extra="旧版自动安装配置已迁移为二进制安装；宿主机和入口虚拟机均可使用以下方式。">
                    <a-radio-group v-model="forms[e.node.id].deploy" @change="checks[e.node.id] = null"><a-radio value="binary">二进制安装</a-radio><a-radio value="docker">Docker</a-radio><a-radio value="manual">已自行安装</a-radio></a-radio-group>
                  </a-form-item>
                  <a-form-item v-if="forms[e.node.id].deploy === 'binary'" label="下载方式" extra="控制端下载并校验 SHA256 后上传，目标机器无需访问外网。大陆网络可主动选择第三方加速；不自动切换下载源。">
                    <a-select v-model="forms[e.node.id].downloadMirror" :options="[{ label: '直连 GitHub（默认）', value: '' }, { label: '大陆加速：ghfast.top（第三方，需信任该服务）', value: 'https://ghfast.top/' }]" />
                  </a-form-item>
                  <a-form-item v-if="forms[e.node.id].deploy === 'docker'" label="OpenResty 镜像" extra="默认从 Docker Hub 拉取官方镜像；大陆网络可显式填写可信镜像仓库完整引用（含标签或 sha256 摘要），不会自动使用第三方镜像。无 Docker 时通过 apt 安装 docker.io。">
                    <a-input v-model="forms[e.node.id].image" placeholder="openresty/openresty:1.31.1.1-0-bookworm-fat" />
                  </a-form-item>
                  <a-form-item label="运行方式">
                    <a-radio-group v-model="forms[e.node.id].mode" class="mode-cards">
                      <a-radio value="host"><div class="t">宿主机运行</div>
                        <div class="d">OpenResty 安装在 PVE 宿主机上，直接监听 Web 端口。部署简单，不占用虚拟机；会在宿主机安装软件。</div></a-radio>
                      <a-radio value="vm"><div class="t">指定虚拟机运行</div>
                        <div class="d">宿主机只把 Web 端口转发到指定虚拟机，OpenResty 在该虚拟机内转发到其他虚拟机。宿主机不装软件，与宿主机隔离。</div></a-radio>
                    </a-radio-group>
                  </a-form-item>
                </a-form>
                <div class="flow" style="margin-bottom:16px">
                  <div class="box">客户端<div class="muted">http(s)://子域名.主域名</div></div><span class="arrow">→</span>
                  <template v-if="forms[e.node.id].mode === 'host'">
                    <div class="box hl">{{ e.node.name }} {{ e.node.wanIp4 }}<div class="muted">宿主机 OpenResty 按域名分流</div></div><span class="arrow">→</span>
                  </template>
                  <template v-else>
                    <div class="box dashed">{{ e.node.name }} {{ e.node.wanIp4 }}<div class="muted">公网端口自行转发到入口虚拟机</div></div><span class="arrow">→</span>
                    <div class="box hl">{{ vmLabel(e, forms[e.node.id].vmId) || '入口虚拟机' }}<div class="muted">虚拟机内 OpenResty 按域名分流</div></div><span class="arrow">→</span>
                  </template>
                  <div class="box">目标虚拟机内网 IP:端口</div>
                </div>
                <a-form v-if="forms[e.node.id].mode === 'vm'" :model="forms[e.node.id]" layout="vertical">
                  <a-row :gutter="40">
                    <a-col :xs="24" :md="8"><a-form-item label="入口虚拟机" required extra="需要能访问同一内网的其他虚拟机">
                      <a-select v-model="forms[e.node.id].vmId" placeholder="选择虚拟机" allow-search @change="checks[e.node.id] = null">
                        <a-option v-for="v in e.vms" :key="v.id" :value="v.id" :label="`${v.vmid} ${v.name}（${v.ipv4 || '未分配 IP'}）`" :disabled="!v.ipv4">
                          <div class="ip-opt"><span><span class="mono">{{ v.vmid }}</span> {{ v.name }}</span><span class="mono muted">{{ v.ipv4 || '未分配 IP' }}</span></div></a-option>
                      </a-select></a-form-item></a-col>
                    <a-col :xs="24" :md="8"><a-form-item label="SSH 连接" :extra="`由节点 ${e.node.name} 连接虚拟机，虚拟机的 SSH 不需要对外开放`">
                      <a-input-group style="width:100%"><a-input v-model="forms[e.node.id].sshUser" style="width:90px" placeholder="用户" />
                        <a-input-number v-model="forms[e.node.id].sshPort" :min="1" :max="65535" style="width:90px"><template #prefix>:</template></a-input-number>
                        <a-select v-model="forms[e.node.id].sshAuth" style="flex:1" :options="[{ label: 'SSH 凭证', value: 'cred' }, { label: '宿主机密钥', value: 'node' }, { label: '密码', value: 'password' }, { label: '私钥', value: 'key' }]" /></a-input-group>
                    </a-form-item></a-col>
                  </a-row>
                  <a-row v-if="forms[e.node.id].sshAuth === 'cred'" :gutter="40"><a-col :xs="24" :md="12">
                    <a-form-item label="SSH 凭证" required extra="在「PVE 节点 → SSH 凭证」中管理；使用凭证中的用户名">
                      <a-select v-model="forms[e.node.id].credId" placeholder="选择凭证" :options="creds.map(c => ({ label: `${c.name}（${c.username} · ${c.type === 'key' ? '私钥' : '密码'}）`, value: c.id }))">
                        <template #empty><div style="padding:12px;text-align:center" class="muted">还没有 SSH 凭证</div></template></a-select>
                    </a-form-item></a-col></a-row>
                  <a-row v-else-if="forms[e.node.id].sshAuth !== 'node'" :gutter="40"><a-col :xs="24" :md="12">
                    <a-form-item :label="forms[e.node.id].sshAuth === 'password' ? 'SSH 密码' : 'SSH 私钥'" :extra="hasSecret(e) ? '已保存，留空则不修改' : '加密保存'">
                      <a-input-password v-if="forms[e.node.id].sshAuth === 'password'" v-model="forms[e.node.id].sshSecret" :placeholder="hasSecret(e) ? '留空不修改' : ''" />
                      <a-textarea v-else v-model="forms[e.node.id].sshSecret" class="mono" spellcheck="false" :auto-size="{ minRows: 2, maxRows: 5 }" :placeholder="hasSecret(e) ? '留空不修改' : '-----BEGIN OPENSSH PRIVATE KEY-----'" />
                    </a-form-item></a-col></a-row>
                  <a-alert v-if="forms[e.node.id].sshAuth === 'node'" style="margin-bottom:16px">
                    使用宿主机的 Web 转发专用密钥登录虚拟机：把下面的公钥加入虚拟机 {{ forms[e.node.id].sshUser }} 用户的 ~/.ssh/authorized_keys
                    <div v-if="pubkeys[e.node.id]" class="cmd mono" style="margin-top:8px">{{ pubkeys[e.node.id] }}
                      <a-button class="copy" size="mini" @click="copyText(pubkeys[e.node.id])">复制</a-button></div>
                    <a-button v-else size="mini" style="margin-top:8px" :loading="pubkeys[e.node.id] === null" @click="loadPubkey(e)">获取宿主机公钥</a-button>
                  </a-alert>
                  <a-alert type="info" style="margin-bottom:16px">
                    OpenResty 在入口虚拟机内监听下面的 Web 端口，并按域名转发到用户的虚拟机；本系统不会改动宿主机端口。公网如何到达入口虚拟机由你自行配置（例如在端口转发中添加公共规则，把公网 80 / 443 转发到入口虚拟机）。
                    <div v-if="vmEntryRules(e).length" style="margin-top:4px">当前宿主机端口转发：{{ vmEntryRules(e).map(r => `${r.port} → ${vmLabel(e, r.vmId) || '虚拟机'}:${r.targetPort}`).join('；') }}</div>
                    <div style="margin-top:4px">入口虚拟机停止时，该节点的全部 Web 转发都会中断，建议使用开机自启、不常重启的虚拟机。</div>
                  </a-alert>
                </a-form>
                <a-form :model="forms[e.node.id]" layout="vertical">
                  <a-row :gutter="40">
                    <a-col :xs="24" :md="8"><a-form-item label="HTTP 端口"><a-input-tag v-model="forms[e.node.id].httpText" placeholder="输入端口后回车" allow-clear @change="v => onPorts(e, 'http', v)" /></a-form-item></a-col>
                    <a-col :xs="24" :md="8"><a-form-item label="HTTPS 端口"><a-input-tag v-model="forms[e.node.id].httpsText" placeholder="输入端口后回车" allow-clear @change="v => onPorts(e, 'https', v)" /></a-form-item></a-col>
                    <a-col :xs="24" :md="8"><a-form-item label="默认入口端口" extra="用户添加站点时默认选中">
                      <a-select v-model="forms[e.node.id].default" :options="allPorts(e).map(p => ({ label: `${forms[e.node.id].https.includes(p) ? 'HTTPS' : 'HTTP'} ${p}`, value: p }))" /></a-form-item></a-col>
                  </a-row>
                </a-form>
                <a-alert v-if="conflicts(e).length" type="warning" style="margin-bottom:8px" title="以下宿主机端口正被端口转发规则使用，部署时将由 Web 转发接管（删除这些规则）">
                  <div v-for="c in conflicts(e)" :key="c.id" class="conflict-row">
                    <span>端口 <b class="mono">{{ c.port }}</b>「{{ c.remark || c.port }}」（{{ c.owner }}）→ <span class="mono">{{ vmLabel(e, c.vmId) || '虚拟机' }}:{{ c.targetPort }}</span></span>
                    <a-link @click="dropPort(e, c.port)">不使用此端口</a-link>
                  </div>
                  <div class="muted" style="margin-top:4px">接管后，这些端口的访问将按域名转发；如果仍要保留原来的端口转发，请点「不使用此端口」把它从 Web 端口中移除。</div>
                </a-alert>
                <div v-if="checks[e.node.id]" class="checks">
                  <div v-for="c in checks[e.node.id]" :key="c.name" :class="c.ok ? 'ok' : 'no'"><icon-check-circle-fill v-if="c.ok" /><icon-close-circle-fill v-else /> {{ c.name }}<span v-if="c.detail" class="muted" style="margin-left:6px">{{ c.detail }}</span></div>
                </div>
              </template>
              <span v-else class="muted">{{ e.node.initialized ? '未启用：该节点上不能添加 Web 转发' : '节点尚未完成网络初始化' }}</span>
              <a-row justify="space-between" align="center" style="margin-top:16px">
                <span class="muted">{{ forms[e.node.id].enabled ? (forms[e.node.id].mode === 'host' ? 'Web 端口自动成为该节点的保留端口，不能再用于端口转发或分配给用户。' : '') : e.state.location ? '保存后将停止该节点的 Web 转发（站点保留，重新启用后恢复）' : '' }}</span>
                <a-space>
                  <a-button v-if="forms[e.node.id].enabled" :loading="checking === e.node.id" @click="check(e)"><template #icon><icon-check-circle /></template>检测环境</a-button>
                  <a-popconfirm v-if="forms[e.node.id].enabled && conflicts(e).length" type="warning" @ok="deploy(e, true)"
                    :content="`部署后将删除端口 ${conflicts(e).map(c => c.port).join('、')} 的端口转发规则，改由 Web 转发处理，确定继续？`">
                    <a-button type="primary">保存并部署</a-button></a-popconfirm>
                  <a-button v-else-if="forms[e.node.id].enabled || e.state.location" type="primary" @click="deploy(e)">{{ forms[e.node.id].enabled ? '保存并部署' : '保存' }}</a-button>
                </a-space>
              </a-row>
            </div>
          </a-space>
        </a-tab-pane>
      </a-tabs>
    </a-card>

    <!-- 主域名 -->
    <a-modal v-model:visible="domainVisible" :title="df.id ? '编辑主域名' : '添加主域名'" :width="560" :ok-loading="saving" @before-ok="saveDomain">
      <a-form :model="df" :label-col-props="{ span: 6 }" :wrapper-col-props="{ span: 18 }">
        <a-form-item label="DNS 服务商" required extra="用户保存站点后，用该账号自动添加 A 记录">
          <a-select v-model="df.providerId" :disabled="!!df.sites" placeholder="选择 DNS 服务商" :options="providers.map(p => ({ label: `${p.name}（${p.typeName}）`, value: p.id }))" @change="loadZones" /></a-form-item>
        <a-form-item label="主域名" required :extra="zonesErr || (df.id ? '用户填写前缀，组成 前缀.主域名' : '从服务商账号获取，可选择多个（也可输入后回车）；保存后逐个添加')"
                     :validate-status="zonesErr ? 'error' : undefined">
          <a-input v-if="df.id" v-model="df.name" :disabled="!!df.sites" />
          <a-select v-else v-model="df.names" multiple allow-create allow-search :loading="zonesLoading" :disabled="!df.providerId"
                    :placeholder="df.providerId ? '选择或输入域名' : '请先选择 DNS 服务商'">
            <a-option v-for="z in zones" :key="z.name" :value="z.name" :disabled="z.added">{{ z.name }}<span v-if="z.added" class="muted">（已添加）</span></a-option>
          </a-select>
        </a-form-item>
        <a-form-item label="可用用户">
          <div style="width:100%"><a-radio-group v-model="df.who"><a-radio value="all">全部用户</a-radio><a-radio value="some">指定用户</a-radio></a-radio-group>
            <a-select v-if="df.who === 'some'" v-model="df.users" multiple style="margin-top:8px" placeholder="选择用户" :options="users.filter(u => u.role !== 'admin').map(u => ({ label: u.username, value: u.id }))" /></div></a-form-item>
      </a-form>
    </a-modal>

    <!-- DNS 服务商 -->
    <a-modal v-model:visible="providerVisible" :title="pf.id ? '编辑 DNS 服务商' : '添加 DNS 服务商'" :width="560" :ok-loading="saving" @before-ok="saveProvider">
      <a-form :model="pf" :label-col-props="{ span: 6 }" :wrapper-col-props="{ span: 18 }">
        <a-form-item label="类型"><a-select v-model="pf.type" :disabled="!!pf.id" :options="types.map(t => ({ label: t.name, value: t.type }))" /></a-form-item>
        <a-form-item label="名称"><a-input v-model="pf.name" placeholder="如 公司 DNSPod 账号（可不填）" /></a-form-item>
        <a-form-item v-for="fd in typeOf(pf.type)?.fields || []" :key="fd.key" :label="fd.label" :required="!fd.optional && !(pf.id && fd.secret)" :extra="fd.hint">
          <a-input-password v-if="fd.secret" v-model="pf.fields[fd.key]" :placeholder="pf.id ? '已保存，留空则不修改' : ''" />
          <a-input v-else v-model="pf.fields[fd.key]" />
        </a-form-item>
        <a-form-item><a-space><a-button :loading="testing === 'form'" @click="testForm">测试连接</a-button>
          <span v-if="testResult" :style="{ color: testResult.ok ? 'rgb(var(--green-6))' : 'rgb(var(--red-6))', fontSize: '12px' }">{{ testResult.text }}</span></a-space></a-form-item>
      </a-form>
    </a-modal>

    <!-- 证书 -->
    <a-modal v-model:visible="certVisible" :title="cf.id ? '编辑证书' : '添加证书'" :width="640" :ok-loading="saving" @before-ok="saveCert">
      <a-form :model="cf" layout="vertical">
        <a-form-item label="名称" required><a-input v-model="cf.name" :max-length="40" placeholder="例如 example 通配符证书" /></a-form-item>
        <a-form-item label="证书（PEM，含中间证书）" required extra="从本地 .pem / .crt / .cer 文件读取内容（不会上传文件），或直接粘贴">
          <div style="width:100%">
            <div class="pem-read"><label><input type="file" hidden accept=".pem,.crt,.cer" @change="ev => readPem(ev, 'cert')" />
              <a-button size="small" @click="ev => ev.currentTarget.previousElementSibling.click()"><template #icon><icon-file /></template>读取证书文件</a-button></label>
              <span v-if="cf.certFile" class="muted" style="margin-left:8px">已读取 {{ cf.certFile }}</span></div>
            <a-textarea v-model="cf.cert" class="mono" spellcheck="false" :auto-size="{ minRows: 5, maxRows: 8 }" placeholder="-----BEGIN CERTIFICATE-----" />
          </div>
        </a-form-item>
        <a-form-item label="私钥（PEM）" required extra="从本地 .key / .pem 文件读取内容（不会上传文件），或直接粘贴；私钥加密保存，只有管理员能查看">
          <div style="width:100%">
            <div class="pem-read"><label><input type="file" hidden accept=".key,.pem" @change="ev => readPem(ev, 'key')" />
              <a-button size="small" @click="ev => ev.currentTarget.previousElementSibling.click()"><template #icon><icon-file /></template>读取私钥文件</a-button></label>
              <span v-if="cf.keyFile" class="muted" style="margin-left:8px">已读取 {{ cf.keyFile }}</span></div>
            <a-textarea v-model="cf.key" class="mono" spellcheck="false" :auto-size="{ minRows: 4, maxRows: 6 }" placeholder="-----BEGIN PRIVATE KEY-----" />
          </div>
        </a-form-item>
        <a-alert v-if="cf.info" :type="expType(cf.info.days)">{{ cf.id && !certChanged ? '当前证书' : '已识别' }}：包含 <b class="mono">{{ cf.info.domains.join('、') }}</b>，颁发者 {{ cf.info.issuer || '-' }}，有效期至 {{ fmtDate(cf.info.notAfter) }}（{{ cf.info.days }} 天）</a-alert>
        <a-alert v-else-if="cf.parseErr" type="error">{{ cf.parseErr }}</a-alert>
      </a-form>
    </a-modal>

    <!-- 部署进度 -->
    <a-modal v-model:visible="deploying.visible" :title="deploying.title" :footer="deploying.done" :mask-closable="false" :closable="deploying.done" hide-cancel ok-text="完成" @ok="deploying.visible = false">
      <div class="deploy-steps">
        <div v-for="(s, i) in deploying.steps" :key="i" :class="s.ok ? 'ok' : 'no'">
          <icon-check-circle-fill v-if="s.ok" /><icon-close-circle-fill v-else /> {{ s.name }}<div v-if="s.error" class="err">{{ s.error }}</div></div>
        <div v-if="!deploying.done" class="wait"><icon-loading /> 正在执行（安装 OpenResty 可能需要几分钟）…</div>
        <a-alert v-if="deploying.error" type="error" style="margin-top:12px">{{ deploying.error }}</a-alert>
        <a-alert v-else-if="deploying.done" type="success" style="margin-top:12px">完成</a-alert>
      </div>
    </a-modal>
  </div>
</template>

<script setup>
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { Message } from '@arco-design/web-vue'
import { api, copyText, expColor, expType, readTextFile, streamNDJSON } from '../api'

const route = useRoute()
const tab = ref(['domains', 'dns', 'certs', 'entry'].includes(route.query.tab) ? route.query.tab : 'domains')
const loading = ref(false)
const loaded = ref(false)
const saving = ref(false)
const domains = ref([])
const providers = ref([])
const types = ref([])
const certs = ref([])
const users = ref([])
const creds = ref([])
const pubkeys = reactive({})
const entries = ref([])
const forms = reactive({})
const checks = reactive({})
const checking = ref(0)
const testing = ref(0)

const fmtDate = ts => { if (!ts) return '-'; const d = typeof ts === 'number' ? new Date(ts * 1000) : new Date(ts); const p = n => String(n).padStart(2, '0'); return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}` }
const typeOf = t => types.value.find(x => x.type === t)

async function load() {
  loading.value = true
  try {
    const [d, p, t, c, u, e, cr] = await Promise.all([api.get('/api/web-domains'), api.get('/api/dns-providers'), api.get('/api/dns-types'),
      api.get('/api/certs'), api.get('/api/users'), api.get('/api/web-entries'), api.get('/api/credentials')])
    domains.value = d; providers.value = p; types.value = t; certs.value = c; users.value = u; creds.value = cr
    setEntries(e)
    loaded.value = true
  } finally { loading.value = false }
}
onMounted(load)
async function del(url) { await api.del(url); Message.success('已删除'); load() }

// ---------------- 主域名 ----------------
const domainVisible = ref(false)
const df = reactive({})
const zones = ref([])
const zonesLoading = ref(false)
const zonesErr = ref('')
async function loadZones() {
  zones.value = []
  zonesErr.value = ''
  if (!df.providerId || df.id) return
  df.names = []
  zonesLoading.value = true
  try {
    zones.value = await api.get(`/api/dns-providers/${df.providerId}/zones`, { silent: true })
    if (!zones.value.length) zonesErr.value = '该账号下没有域名'
  } catch (e) { zonesErr.value = e.message } finally { zonesLoading.value = false }
}
function openDomain(r) {
  Object.assign(df, r ? { id: r.id, name: r.name, names: [], providerId: r.providerId, who: r.users.length ? 'some' : 'all', users: [...r.users], sites: r.sites }
    : { id: 0, name: '', names: [], providerId: providers.value.length === 1 ? providers.value[0].id : undefined, who: 'all', users: [], sites: 0 })
  zones.value = []
  zonesErr.value = ''
  domainVisible.value = true
  loadZones()
}
async function saveDomain(done) {
  if (!df.id && !df.names.length) { Message.warning('请选择或输入主域名'); return done(false) }
  saving.value = true
  try {
    const users = df.who === 'some' ? df.users : []
    if (df.id) await api.put(`/api/web-domains/${df.id}`, { name: df.name, providerId: df.providerId, users })
    else await api.post('/api/web-domains', { names: df.names, providerId: df.providerId, users })
    Message.success(df.id ? '已保存主域名' : `已添加 ${df.names.length} 个主域名`)
    load(); done(true)
  } catch { done(false) } finally { saving.value = false }
}

// ---------------- DNS 服务商 ----------------
const providerVisible = ref(false)
const pf = reactive({ fields: {} })
const testResult = ref(null)
function openProvider(r) {
  Object.assign(pf, r ? { id: r.id, type: r.type, name: r.name, fields: {} } : { id: 0, type: types.value[0]?.type, name: '', fields: {} })
  testResult.value = null
  providerVisible.value = true
}
watch(() => pf.type, () => { testResult.value = null })
async function testForm() {
  testing.value = 'form'
  try {
    const r = await api.post('/api/dns-providers/test', { id: pf.id, type: pf.type, fields: pf.fields })
    testResult.value = r.ok ? { ok: true, text: `连接成功，账号下共 ${r.zones?.length || 0} 个域名` } : { ok: false, text: r.error }
  } catch (e) { testResult.value = { ok: false, text: e.message } } finally { testing.value = 0 }
}
async function testSaved(r) {
  testing.value = r.id
  try {
    const x = await api.post(`/api/dns-providers/${r.id}/test`, {})
    if (x.ok) Message.success(`${r.name}：连接成功，账号下共 ${x.zones?.length || 0} 个域名`)
    else Message.error(`${r.name}：${x.error}`)
    load()
  } finally { testing.value = 0 }
}
async function saveProvider(done) {
  saving.value = true
  try {
    const body = { type: pf.type, name: pf.name, fields: pf.fields }
    const r = pf.id ? await api.put(`/api/dns-providers/${pf.id}`, body) : await api.post('/api/dns-providers', body)
    if (r.status) Message.warning({ content: `已保存，但连接测试失败：${r.status}`, duration: 6000 })
    else Message.success(`已加密保存，账号下共 ${r.zones} 个域名`)
    load(); done(true)
  } catch { done(false) } finally { saving.value = false }
}

// ---------------- 证书 ----------------
const certVisible = ref(false)
const cf = reactive({})
let origPem = ''
const certChanged = computed(() => cf.cert?.trim() !== origPem.trim())
async function openCert(r) {
  Object.assign(cf, { id: r?.id || 0, name: r?.name || '', cert: '', key: '', certFile: '', keyFile: '', info: null, parseErr: '' })
  origPem = ''
  if (r) {
    const d = await api.get(`/api/certs/${r.id}`)
    cf.cert = d.pem; cf.key = d.key; origPem = d.pem
  }
  certVisible.value = true
}
let parseTimer
watch(() => [cf.cert, cf.key], () => {
  clearTimeout(parseTimer)
  if (!cf.cert?.trim()) { cf.info = null; cf.parseErr = ''; return }
  parseTimer = setTimeout(async () => {
    try {
      cf.info = await api.post('/api/certs/parse', { cert: cf.cert, key: cf.key }, { silent: true })
      cf.parseErr = ''
    } catch (e) { cf.info = null; cf.parseErr = e.message }
  }, 400)
})
async function readPem(ev, k) {
  const file = ev.target.files[0]
  ev.target.value = ''
  if (!file) return
  try {
    const t = await readTextFile(file)
    const ok = k === 'cert' ? t.includes('BEGIN CERTIFICATE') : /BEGIN (RSA |EC |)PRIVATE KEY/.test(t)
    if (!ok) { Message.error(k === 'cert' ? '不是 PEM 格式的证书' : '不是 PEM 格式的私钥'); return }
    cf[k] = t.trim(); cf[k + 'File'] = file.name
  } catch (e) { Message.error(e.message) }
}
async function saveCert(done) {
  if (!cf.name?.trim() || !cf.cert?.trim() || !cf.key?.trim()) { Message.warning('请填写名称、证书和私钥'); return done(false) }
  if (cf.parseErr) { Message.warning(cf.parseErr); return done(false) }
  saving.value = true
  try {
    const body = { name: cf.name, cert: cf.cert, key: cf.key }
    if (cf.id) await api.put(`/api/certs/${cf.id}`, body)
    else await api.post('/api/certs', body)
    Message.success(cf.id && certChanged.value ? '已更换证书，使用它的站点会自动重新加载' : '已保存证书')
    load(); done(true)
  } catch { done(false) } finally { saving.value = false }
}

// ---------------- Web 入口 ----------------
function setEntries(list) {
  entries.value = list
  for (const e of list) {
    const w = e.web
    forms[e.node.id] = { enabled: w.enabled, mode: w.mode || 'host', vmId: w.vmId || undefined, deploy: !w.deploy || w.deploy === 'auto' ? 'binary' : w.deploy, image: w.image || 'openresty/openresty:1.31.1.1-0-bookworm-fat', downloadMirror: w.downloadMirror || '', sshUser: w.sshUser || 'root', credId: w.credId || undefined,
      sshPort: w.sshPort || 22, sshAuth: w.sshAuth || 'node', sshSecret: '', http: [...(w.http || [])], https: [...(w.https || [])],
      httpText: (w.http || []).map(String), httpsText: (w.https || []).map(String), default: w.default }
  }
}
const hasSecret = e => e.hasSecret && e.web.sshAuth === forms[e.node.id].sshAuth
const allPorts = e => [...forms[e.node.id].http, ...forms[e.node.id].https].sort((a, b) => a - b)
const vmLabel = (e, id) => { const v = e.vms.find(x => x.id === id); return v ? `${v.vmid} ${v.name} ${v.ipv4}` : '' }
const locName = (e, loc) => (loc.startsWith('vm:') ? `虚拟机 ${vmLabel(e, Number(loc.slice(3))) || loc.slice(3)}` : '宿主机')
const shortVersion = v => (v || '').replace(/^nginx version:\s*/, '') || 'OpenResty'
function onPorts(e, kind, vals) {
  const f = forms[e.node.id]
  const nums = []
  for (const v of vals) {
    const n = Number(String(v).trim())
    if (!Number.isInteger(n) || n < 1 || n > 65535) { Message.warning(`“${v}” 不是有效端口`); continue }
    if (nums.includes(n) || (kind === 'http' ? f.https : f.http).includes(n)) { Message.warning(`端口 ${n} 重复`); continue }
    nums.push(n)
  }
  f[kind] = nums
  f[kind + 'Text'] = nums.map(String)
  if (!allPorts(e).includes(f.default)) f.default = allPorts(e)[0]
}
function dropPort(e, p) {
  const f = forms[e.node.id]
  f.http = f.http.filter(x => x !== p); f.https = f.https.filter(x => x !== p)
  f.httpText = f.http.map(String); f.httpsText = f.https.map(String)
  if (f.default === p) f.default = allPorts(e)[0]
}
// 宿主机运行时，与 Web 端口冲突的端口转发规则（入口虚拟机模式不使用宿主机端口）
const conflicts = e => (forms[e.node.id]?.enabled && forms[e.node.id].mode === 'host' ? e.rules.filter(r => allPorts(e).includes(r.port)) : [])
// 入口虚拟机模式：宿主机上与 Web 端口同号的端口转发（仅展示，便于核对公网是否已转发到入口虚拟机）
const vmEntryRules = e => e.rules.filter(r => allPorts(e).includes(r.port))
function webBody(e) {
  const f = forms[e.node.id]
  return { enabled: f.enabled, mode: f.mode, vmId: f.vmId || 0, deploy: f.deploy, image: f.image, downloadMirror: f.downloadMirror, sshUser: f.sshUser, sshPort: f.sshPort, sshAuth: f.sshAuth, sshSecret: f.sshSecret, credId: f.credId || 0,
    http: f.http, https: f.https, default: f.default }
}
async function loadPubkey(e) {
  pubkeys[e.node.id] = null
  try { pubkeys[e.node.id] = (await api.post(`/api/nodes/${e.node.id}/web/pubkey`)).publicKey } catch { delete pubkeys[e.node.id] }
}
async function check(e) {
  checking.value = e.node.id
  try {
    const r = await api.post(`/api/nodes/${e.node.id}/web/check`, webBody(e))
    checks[e.node.id] = r.checks || []
    if (checks[e.node.id].every(c => c.ok)) Message.success('环境检测通过')
  } finally { checking.value = 0 }
}
const deploying = reactive({ visible: false, title: '', steps: [], done: false, error: '' })
async function deploy(e, takeOver = false) {
  const f = forms[e.node.id]
  if (f.enabled && f.mode === 'vm' && !f.vmId) { Message.warning('请选择入口虚拟机'); return }
  Object.assign(deploying, { visible: true, title: `${f.enabled ? '部署' : '停用'} ${e.node.name} 的 Web 转发`, steps: [], done: false, error: '' })
  try {
    await streamNDJSON(`/api/nodes/${e.node.id}/web`, { ...webBody(e), takeOver }, m => {
      if (m.step) deploying.steps.push(m.step)
      if (m.error) deploying.error = m.error
    })
  } catch (err) { deploying.error = err.message }
  deploying.done = true
  const fresh = await api.get('/api/web-entries')
  setEntries(fresh)
}
</script>

<style scoped>
.entry-card { border: 1px solid var(--color-border-2); border-radius: 4px; padding: 16px 20px; }
.mode-cards { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; width: 100%; }
.mode-cards :deep(.arco-radio) { margin: 0; padding: 12px 14px; border: 1px solid var(--color-border-2); border-radius: 4px; align-items: flex-start; }
.mode-cards :deep(.arco-radio-checked) { border-color: rgb(var(--primary-6)); background: rgb(var(--primary-1)); }
.mode-cards .t { font-weight: 500; margin-bottom: 4px; }
.mode-cards .d { font-size: 12px; color: var(--color-text-3); line-height: 1.7; white-space: normal; }
.flow { display: flex; align-items: center; justify-content: center; gap: 12px; padding: 16px; background: var(--color-fill-1); border-radius: 4px; font-size: 13px; flex-wrap: wrap; }
.flow .box { padding: 8px 12px; background: var(--color-bg-2); border: 1px solid var(--color-border-2); border-radius: 4px; text-align: center; line-height: 1.6; }
.flow .box.dashed { border-style: dashed; }
.flow .box.hl { border-color: rgb(var(--primary-6)); background: rgb(var(--primary-1)); }
.flow .arrow { color: var(--color-text-3); }
.checks { display: grid; grid-template-columns: 1fr 1fr; gap: 6px 24px; padding: 12px 16px; margin-top: 8px; background: var(--color-fill-1); border-radius: 4px; font-size: 13px; }
.checks .ok { color: rgb(var(--green-6)); }
.checks .no { color: rgb(var(--red-6)); }
.pem-read { margin-bottom: 8px; }
.deploy-steps { line-height: 2; }
.deploy-steps .ok { color: rgb(var(--green-6)); }
.deploy-steps .no { color: rgb(var(--red-6)); }
.deploy-steps .err { margin-left: 22px; color: var(--color-text-2); font-size: 12px; line-height: 1.6; white-space: pre-wrap; word-break: break-all; }
.deploy-steps .wait { color: var(--color-text-3); }
</style>
