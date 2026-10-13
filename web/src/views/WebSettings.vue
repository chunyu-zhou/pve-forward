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
              <a-row justify="space-between" align="center">
                <a-space wrap>
                  <b>{{ e.node.name }}</b><span class="muted mono">{{ e.node.wanIp4 }}</span>
                  <template v-if="e.web.enabled && e.state.location">
                    <a-badge v-if="e.report && !e.report.up" status="danger" :text="e.report.error || 'OpenResty 未运行'" />
                    <a-badge v-else status="success" :text="`运行中 · ${shortVersion(e.state.version)}`" />
                  </template>
                  <a-tag v-else-if="e.web.enabled" color="orange" size="small">未部署</a-tag>
                  <a-tag v-else size="small">未启用</a-tag>
                </a-space>
                <a-space>
                  <template v-if="e.node.initialized">
                    <a-button type="primary" size="small" @click="openWizard(e)">{{ e.web.enabled ? '修改配置' : '启用 Web 转发' }}</a-button>
                    <a-popconfirm v-if="e.web.enabled" type="warning" content="停用后该节点的站点停止访问（站点保留，重新启用后恢复），确定停用？" @ok="disable(e)">
                      <a-button size="small" status="danger">停用</a-button></a-popconfirm>
                  </template>
                  <span v-else class="muted">节点尚未完成网络初始化</span>
                </a-space>
              </a-row>
              <a-descriptions v-if="e.web.enabled" class="entry-sum" :column="{ xs: 1, md: 4 }" size="small" :data="[
                { label: '运行位置', value: e.web.mode === 'vm' ? `入口虚拟机 ${vmLabel(e, e.web.vmId) || ''}` : '宿主机' },
                { label: 'OpenResty', value: e.state.location ? shortVersion(e.state.version) : '未部署' },
                { label: 'HTTP 端口', value: (e.web.http || []).join('、') || '-' },
                { label: 'HTTPS 端口', value: (e.web.https || []).join('、') || '-' } ]" />
              <a-alert v-if="e.state.error" type="error" style="margin-top:12px" title="最近一次配置下发失败（站点保持原配置运行）">
                {{ e.state.error }}
                <template #action><a-button size="mini" @click="openWizard(e, true)">重试</a-button></template>
              </a-alert>
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

    <!-- Web 入口：分步配置并部署 -->
    <a-modal v-model:visible="wz.visible" :title="wz.e ? `${wz.f.enabled === false ? '停用' : '配置'} ${wz.e.node.name} 的 Web 转发` : ''" :width="860" :footer="false"
             :mask-closable="false" :closable="!wz.running" unmount-on-close>
      <template v-if="wz.e">
        <a-steps v-if="wz.f.enabled !== false" :current="wz.step + 1" size="small" style="margin-bottom:24px">
          <a-step>运行位置</a-step><a-step>OpenResty</a-step><a-step>Web 端口</a-step><a-step>确认部署</a-step>
        </a-steps>

        <!-- 1. 运行位置 -->
        <div v-show="wz.step === 0">
          <a-radio-group v-model="wz.f.mode" class="mode-cards">
            <a-radio value="host"><div class="t">宿主机运行</div>
              <div class="d">OpenResty 运行在 PVE 宿主机上，直接监听 Web 端口。部署简单，不占用虚拟机。</div></a-radio>
            <a-radio value="vm"><div class="t">指定虚拟机运行</div>
              <div class="d">OpenResty 运行在入口虚拟机内，按域名转发到其他虚拟机。宿主机不装软件；公网到入口虚拟机的转发由你自行配置。</div></a-radio>
          </a-radio-group>
          <div class="flow" style="margin:16px 0">
            <div class="box">客户端<div class="muted">http(s)://子域名.主域名</div></div><span class="arrow">→</span>
            <template v-if="wz.f.mode === 'host'">
              <div class="box hl">{{ wz.e.node.name }} {{ wz.e.node.wanIp4 }}<div class="muted">宿主机 OpenResty 按域名分流</div></div><span class="arrow">→</span>
            </template>
            <template v-else>
              <div class="box dashed">{{ wz.e.node.name }} {{ wz.e.node.wanIp4 }}<div class="muted">公网端口自行转发到入口虚拟机</div></div><span class="arrow">→</span>
              <div class="box hl">{{ vmLabel(wz.e, wz.f.vmId) || '入口虚拟机' }}<div class="muted">虚拟机内 OpenResty 按域名分流</div></div><span class="arrow">→</span>
            </template>
            <div class="box">目标虚拟机内网 IP:端口</div>
          </div>
          <a-form v-if="wz.f.mode === 'vm'" :model="wz.f" layout="vertical">
            <a-row :gutter="24">
              <a-col :xs="24" :md="12"><a-form-item label="入口虚拟机" required extra="需要能访问同一内网的其他虚拟机；建议开机自启、不常重启">
                <a-select v-model="wz.f.vmId" placeholder="选择虚拟机" allow-search>
                  <a-option v-for="v in wz.e.vms" :key="v.id" :value="v.id" :label="`${v.vmid} ${v.name}（${v.ipv4 || '未分配 IP'}）`" :disabled="!v.ipv4">
                    <div class="ip-opt"><span><span class="mono">{{ v.vmid }}</span> {{ v.name }}</span><span class="mono muted">{{ v.ipv4 || '未分配 IP' }}</span></div></a-option>
                </a-select></a-form-item></a-col>
              <a-col :xs="24" :md="12"><a-form-item label="SSH 连接" :extra="`由节点 ${wz.e.node.name} 连接虚拟机，SSH 不需要对外开放`">
                <a-input-group style="width:100%"><a-input v-model="wz.f.sshUser" style="width:90px" placeholder="用户" />
                  <a-input-number v-model="wz.f.sshPort" :min="1" :max="65535" style="width:90px"><template #prefix>:</template></a-input-number>
                  <a-select v-model="wz.f.sshAuth" style="flex:1" :options="[{ label: 'SSH 凭证', value: 'cred' }, { label: '宿主机密钥', value: 'node' }, { label: '密码', value: 'password' }, { label: '私钥', value: 'key' }]" /></a-input-group>
              </a-form-item></a-col>
            </a-row>
            <a-form-item v-if="wz.f.sshAuth === 'cred'" label="SSH 凭证" required extra="在「PVE 节点 → SSH 凭证」中管理；使用凭证中的用户名">
              <a-select v-model="wz.f.credId" placeholder="选择凭证" :options="creds.map(c => ({ label: `${c.name}（${c.username} · ${c.type === 'key' ? '私钥' : '密码'}）`, value: c.id }))">
                <template #empty><div style="padding:12px;text-align:center" class="muted">还没有 SSH 凭证</div></template></a-select>
            </a-form-item>
            <a-form-item v-else-if="wz.f.sshAuth !== 'node'" :label="wz.f.sshAuth === 'password' ? 'SSH 密码' : 'SSH 私钥'" :extra="hasSecret(wz.e, wz.f) ? '已保存，留空则不修改' : '加密保存'">
              <a-input-password v-if="wz.f.sshAuth === 'password'" v-model="wz.f.sshSecret" :placeholder="hasSecret(wz.e, wz.f) ? '留空不修改' : ''" />
              <a-textarea v-else v-model="wz.f.sshSecret" class="mono" spellcheck="false" :auto-size="{ minRows: 2, maxRows: 5 }" :placeholder="hasSecret(wz.e, wz.f) ? '留空不修改' : '-----BEGIN OPENSSH PRIVATE KEY-----'" />
            </a-form-item>
            <a-alert v-else>
              把宿主机的 Web 转发公钥加入虚拟机 {{ wz.f.sshUser }} 用户的 ~/.ssh/authorized_keys
              <div v-if="pubkeys[wz.e.node.id]" class="cmd mono" style="margin-top:8px">{{ pubkeys[wz.e.node.id] }}
                <a-button class="copy" size="mini" @click="copyText(pubkeys[wz.e.node.id])">复制</a-button></div>
              <a-button v-else size="mini" style="margin-top:8px" :loading="pubkeys[wz.e.node.id] === null" @click="loadPubkey(wz.e)">获取宿主机公钥</a-button>
            </a-alert>
          </a-form>
        </div>

        <!-- 2. OpenResty：先检测，已安装的直接使用，未安装才选择安装方式 -->
        <div v-show="wz.step === 1">
          <div v-if="wz.checking" class="wz-loading"><icon-loading /> 正在检测{{ wz.f.mode === 'vm' ? '入口虚拟机' : '宿主机' }}的环境…</div>
          <template v-else-if="wz.check">
            <div class="checks"><div v-for="c in envChecks" :key="c.name" :class="c.ok ? 'ok' : 'no'"><icon-check-circle-fill v-if="c.ok" /><icon-close-circle-fill v-else /> {{ c.name }}<span v-if="c.detail" class="muted">，{{ c.detail }}</span></div></div>
            <a-alert v-if="wz.check.existing" type="success" style="margin-top:16px" title="已检测到 OpenResty，直接使用，无需选择安装方式">
              {{ existingText(wz.check.existing) }}
            </a-alert>
            <a-form v-else :model="wz.f" layout="vertical" style="margin-top:16px">
              <a-form-item label="安装方式" extra="未检测到已安装的 OpenResty（apt、源码编译、Docker 均可识别），请选择安装方式">
                <a-radio-group v-model="wz.f.deploy" type="button" @change="recheck">
                  <a-radio value="binary">二进制安装</a-radio><a-radio value="docker">Docker</a-radio></a-radio-group>
              </a-form-item>
              <a-form-item v-if="wz.f.deploy === 'binary'" label="下载方式" extra="控制端下载并校验 SHA256 后上传，目标机器无需访问外网">
                <a-select v-model="wz.f.downloadMirror" :options="mirrors.download.map(m => ({ label: m.label, value: m.value }))" />
              </a-form-item>
              <template v-else>
                <a-form-item label="OpenResty 镜像" :extra="registryHint">
                  <a-input-group style="width:100%">
                    <a-input v-model="wz.f.image" style="flex:1" :placeholder="mirrors.defaultImage" />
                    <a-select v-model="wz.f.registry" style="width:250px" :disabled="hasRegistryHost(wz.f.image)" :options="mirrors.registry.map(m => ({ label: m.label, value: m.value }))">
                      <template #prefix>镜像源</template></a-select>
                  </a-input-group>
                </a-form-item>
                <a-form-item v-if="!wz.check.docker" label="安装 Docker" extra="目标机器未安装 Docker，将从所选软件源安装 Docker CE；会校验软件源的 GPG 密钥是否为 Docker 官方密钥">
                  <a-select v-model="wz.f.dockerSource" allow-search :options="mirrors.dockerSource.map(m => ({ label: m.label, value: m.value }))" @change="recheck" />
                </a-form-item>
                <div v-else class="muted">已安装 Docker {{ wz.check.docker }}</div>
              </template>
            </a-form>
          </template>
          <a-alert v-else-if="wz.checkError" type="error">{{ wz.checkError }}</a-alert>
        </div>

        <!-- 3. Web 端口 -->
        <div v-show="wz.step === 2">
          <a-form :model="wz.f" layout="vertical">
            <a-row :gutter="24">
              <a-col :xs="24" :md="8"><a-form-item label="HTTP 端口"><a-input-tag v-model="wz.f.httpText" placeholder="输入端口后回车" allow-clear @change="v => onPorts('http', v)" /></a-form-item></a-col>
              <a-col :xs="24" :md="8"><a-form-item label="HTTPS 端口"><a-input-tag v-model="wz.f.httpsText" placeholder="输入端口后回车" allow-clear @change="v => onPorts('https', v)" /></a-form-item></a-col>
              <a-col :xs="24" :md="8"><a-form-item label="默认入口端口" extra="用户添加站点时默认选中">
                <a-select v-model="wz.f.default" :options="allPorts().map(p => ({ label: `${wz.f.https.includes(p) ? 'HTTPS' : 'HTTP'} ${p}`, value: p }))" /></a-form-item></a-col>
            </a-row>
          </a-form>
          <div class="muted" style="margin:-8px 0 12px">{{ wz.f.mode === 'host' ? 'Web 端口自动成为该节点的保留端口，不能再用于端口转发或分配给用户。' : 'OpenResty 在入口虚拟机内监听这些端口；本系统不改动宿主机端口。' }}</div>
          <a-alert v-if="conflicts.length" type="warning" style="margin-bottom:12px" title="以下宿主机端口正被端口转发规则使用，部署时由 Web 转发接管（删除这些规则）">
            <div v-for="c in conflicts" :key="c.id" class="conflict-row">
              <span>端口 <b class="mono">{{ c.port }}</b>「{{ c.remark || c.port }}」（{{ c.owner }}）→ <span class="mono">{{ vmLabel(wz.e, c.vmId) || '虚拟机' }}:{{ c.targetPort }}</span></span>
              <a-link @click="dropPort(c.port)">不使用此端口</a-link>
            </div>
          </a-alert>
          <div v-if="wz.checking" class="wz-loading"><icon-loading /> 正在检测端口…</div>
          <template v-else-if="wz.check">
            <div v-if="portCheck" class="checks one" :class="portCheck.ok ? 'ok' : 'no'">
              <div :class="portCheck.ok ? 'ok' : 'no'"><icon-check-circle-fill v-if="portCheck.ok" /><icon-close-circle-fill v-else /> {{ portCheck.name }}<span v-if="portCheck.detail" class="muted">，{{ portCheck.detail }}</span></div>
            </div>
            <a-alert v-if="busyContainers.length" type="warning" style="margin-top:12px" title="端口被 Docker 容器占用">
              <div v-for="c in busyContainers" :key="c.name" style="margin-top:4px">
                <a-checkbox :model-value="wz.f.stop.includes(c.name)" @change="v => toggleStop(c.name, v)">
                  部署时停止容器 <b class="mono">{{ c.name }}</b>（占用 {{ c.ports.join('、') }}）</a-checkbox>
              </div>
              <div class="muted" style="margin-top:6px">容器只停止、不删除，并暂时取消其自动重启；停用 Web 转发时恢复原状态。该容器提供的服务（如 Nginx Proxy Manager 中的站点）会在停止期间中断。也可以改用其他端口。</div>
            </a-alert>
          </template>
        </div>

        <!-- 4. 确认并部署 -->
        <div v-show="wz.step === 3">
          <a-descriptions v-if="!wz.started && wz.f.enabled !== false" :column="1" size="small" bordered :data="summary" />
          <div v-if="wz.started" class="deploy-steps">
            <div v-for="(s, i) in wz.steps" :key="i" :class="s.ok ? 'ok' : 'no'">
              <icon-check-circle-fill v-if="s.ok" /><icon-close-circle-fill v-else /> {{ s.name }}<div v-if="s.error" class="err">{{ s.error }}</div></div>
            <div v-if="wz.running" class="wait"><icon-loading /> 正在执行（安装 OpenResty 或 Docker 可能需要几分钟）…</div>
            <pre ref="installLog" class="install-log" tabindex="0" aria-label="部署日志" @scroll="onLogScroll">{{ wz.log || '等待输出…' }}</pre>
            <a-alert v-if="wz.error" type="error" style="margin-top:12px">{{ wz.error }}</a-alert>
            <a-alert v-else-if="!wz.running" type="success" style="margin-top:12px">{{ wz.f.enabled === false ? '已停用' : '部署完成' }}</a-alert>
          </div>
        </div>

        <a-row justify="space-between" style="margin-top:24px">
          <span class="muted">{{ stepHint }}</span>
          <a-space>
            <template v-if="!wz.started">
              <a-button v-if="wz.step > 0" @click="wz.step--">上一步</a-button>
              <a-button v-if="wz.step === 1 || wz.step === 2" :loading="wz.checking" @click="runCheck">重新检测</a-button>
              <a-button v-if="wz.step < 3" type="primary" :disabled="wz.checking" @click="next">下一步</a-button>
              <a-popconfirm v-else-if="conflicts.length" type="warning" :content="`部署后将删除端口 ${conflicts.map(c => c.port).join('、')} 的端口转发规则，改由 Web 转发处理，确定继续？`" @ok="deploy(true)">
                <a-button type="primary">部署</a-button></a-popconfirm>
              <a-button v-else type="primary" @click="deploy(false)">部署</a-button>
            </template>
            <template v-else-if="!wz.running">
              <template v-if="wz.error">
                <a-button v-if="wz.f.enabled !== false" @click="backToEdit">返回修改</a-button>
                <a-button type="primary" @click="deploy(wz.takeOver)"><template #icon><icon-refresh /></template>重试</a-button>
              </template>
              <a-button v-else type="primary" @click="wz.visible = false">完成</a-button>
            </template>
          </a-space>
        </a-row>
      </template>
    </a-modal>
  </div>
</template>

<script setup>
import { computed, nextTick, onMounted, reactive, ref, watch } from 'vue'
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
const mirrors = reactive({ download: [], dockerSource: [], registry: [], defaultImage: '' })
const testing = ref(0)

const fmtDate = ts => { if (!ts) return '-'; const d = typeof ts === 'number' ? new Date(ts * 1000) : new Date(ts); const p = n => String(n).padStart(2, '0'); return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}` }
const typeOf = t => types.value.find(x => x.type === t)

async function load() {
  loading.value = true
  try {
    const [d, p, t, c, u, e, cr, m] = await Promise.all([api.get('/api/web-domains'), api.get('/api/dns-providers'), api.get('/api/dns-types'),
      api.get('/api/certs'), api.get('/api/users'), api.get('/api/web-entries'), api.get('/api/credentials'), api.get('/api/web-mirrors')])
    domains.value = d; providers.value = p; types.value = t; certs.value = c; users.value = u; creds.value = cr
    Object.assign(mirrors, m)
    entries.value = e
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

// ---------------- Web 入口：分步配置 ----------------
const vmLabel = (e, id) => { const v = e.vms.find(x => x.id === id); return v ? `${v.vmid} ${v.name} ${v.ipv4}` : '' }
const shortVersion = v => (v || '').replace(/^nginx version:\s*/, '') || 'OpenResty'
const hasSecret = (e, f) => e.hasSecret && e.web.sshAuth === f.sshAuth
// 镜像是否自带仓库地址（此时不使用镜像源）
function hasRegistryHost(img) {
  const i = (img || '').indexOf('/')
  if (i < 0) return false
  const first = img.slice(0, i)
  return /[.:]/.test(first) || first === 'localhost'
}
function formOf(e) {
  const w = e.web
  return { enabled: true, mode: w.mode || 'host', vmId: w.vmId || undefined, deploy: w.deploy === 'docker' ? 'docker' : 'binary',
    image: w.image || mirrors.defaultImage, downloadMirror: w.downloadMirror || '', dockerSource: w.dockerSource || 'download.docker.com', registry: w.registry || '',
    sshUser: w.sshUser || 'root', credId: w.credId || undefined, sshPort: w.sshPort || 22, sshAuth: w.sshAuth || 'node', sshSecret: '',
    http: [...(w.http || [])], https: [...(w.https || [])], httpText: (w.http || []).map(String), httpsText: (w.https || []).map(String), default: w.default, stop: [] }
}

const wz = reactive({ visible: false, e: null, f: {}, step: 0, check: null, checkKey: '', checkError: '', checking: false, seenBusy: {},
  started: false, running: false, steps: [], log: '', error: '', takeOver: false })
function webBody() {
  const f = wz.f
  return { enabled: f.enabled, mode: f.mode, vmId: f.vmId || 0, deploy: f.deploy, image: f.image, downloadMirror: f.downloadMirror, dockerSource: f.dockerSource, registry: f.registry,
    sshUser: f.sshUser, sshPort: f.sshPort, sshAuth: f.sshAuth, sshSecret: f.sshSecret, credId: f.credId || 0, http: f.http, https: f.https, default: f.default, stop: f.stop }
}
const bodyKey = () => JSON.stringify(webBody())
function resetRun() { Object.assign(wz, { started: false, running: false, steps: [], log: '', error: '' }) }
function openWizard(e, retry = false) {
  Object.assign(wz, { visible: true, e, f: formOf(e), step: retry ? 3 : 0, check: null, checkKey: '', checkError: '', seenBusy: {}, takeOver: false })
  resetRun()
  if (retry) deploy(false)
}
function disable(e) {
  Object.assign(wz, { visible: true, e, f: { ...formOf(e), enabled: false }, step: 3, check: null, checkKey: '' })
  resetRun()
  deploy(false)
}

async function runCheck() {
  wz.checking = true
  wz.checkError = ''
  const key = bodyKey()
  try {
    wz.check = await api.post(`/api/nodes/${wz.e.node.id}/web/check`, webBody(), { silent: true })
    wz.checkKey = key
    for (const b of wz.check.busy || []) {
      if (!b.container) continue
      const ports = wz.seenBusy[b.container] || []
      if (!ports.includes(b.port)) wz.seenBusy[b.container] = [...ports, b.port].sort((x, y) => x - y)
    }
  } catch (err) { wz.check = null; wz.checkError = err.message } finally { wz.checking = false }
}
const recheck = () => { if (wz.step >= 1) runCheck() }
const isPortCheck = c => c.name.startsWith('端口 ')
const envChecks = computed(() => (wz.check?.checks || []).filter(c => !isPortCheck(c)))
const portCheck = computed(() => (wz.check?.checks || []).find(isPortCheck))
// 占用 Web 端口的容器（含已勾选停止、因此不再显示为占用的）
const busyContainers = computed(() => Object.entries(wz.seenBusy).map(([name, ports]) => ({ name, ports })))
function toggleStop(name, on) {
  wz.f.stop = on ? [...new Set([...wz.f.stop, name])] : wz.f.stop.filter(x => x !== name)
  runCheck()
}
function existingText(x) {
  switch (x.kind) {
    case 'binary': return `${x.version}（本系统安装的二进制版）`
    case 'docker': return `${x.version}（Docker 镜像 ${x.path}），部署时使用该镜像运行`
    case 'system': return `${x.version}（${x.path}，如 apt 或源码安装），部署时原主配置备份为 nginx.conf.pvef-orig，停用时恢复`
    default: return `${x.version}（Docker 容器 ${x.container} 的镜像 ${x.image}），部署时使用该镜像运行，无需重新下载`
  }
}
const registryHint = computed(() => {
  const f = wz.f
  if (hasRegistryHost(f.image)) return '镜像地址已包含仓库，不使用镜像源'
  if (!f.registry) return '从 Docker Hub 拉取；大陆网络可在右侧选择镜像源（参考 LinuxMirrors）'
  if (f.image === mirrors.defaultImage) return '通过所选镜像源按官方摘要拉取默认镜像，内容与官方镜像一致'
  return '通过第三方镜像源拉取，请确认信任该服务'
})

const allPorts = () => [...wz.f.http, ...wz.f.https].sort((a, b) => a - b)
function onPorts(kind, vals) {
  const f = wz.f
  const nums = []
  for (const v of vals) {
    const n = Number(String(v).trim())
    if (!Number.isInteger(n) || n < 1 || n > 65535) { Message.warning(`“${v}” 不是有效端口`); continue }
    if (nums.includes(n) || (kind === 'http' ? f.https : f.http).includes(n)) { Message.warning(`端口 ${n} 重复`); continue }
    nums.push(n)
  }
  f[kind] = nums
  f[kind + 'Text'] = nums.map(String)
  if (!allPorts().includes(f.default)) f.default = allPorts()[0]
}
function dropPort(p) {
  const f = wz.f
  f.http = f.http.filter(x => x !== p); f.https = f.https.filter(x => x !== p)
  f.httpText = f.http.map(String); f.httpsText = f.https.map(String)
  if (f.default === p) f.default = allPorts()[0]
}
// 宿主机运行时，与 Web 端口冲突的端口转发规则（入口虚拟机模式不使用宿主机端口）
const conflicts = computed(() => (wz.e && wz.f.mode === 'host' ? wz.e.rules.filter(r => allPorts().includes(r.port)) : []))

async function next() {
  const f = wz.f
  if (wz.step === 0) {
    if (f.mode === 'vm') {
      if (!f.vmId) { Message.warning('请选择入口虚拟机'); return }
      if (f.sshAuth === 'cred' && !f.credId) { Message.warning('请选择 SSH 凭证'); return }
      if ((f.sshAuth === 'password' || f.sshAuth === 'key') && !f.sshSecret && !hasSecret(wz.e, f)) { Message.warning('请填写 SSH 密码或私钥'); return }
    }
    wz.step = 1
    if (wz.checkKey !== bodyKey()) runCheck()
    return
  }
  if (wz.checkKey !== bodyKey() || !wz.check) { await runCheck(); if (!wz.check) return }
  if (wz.step === 1) {
    const bad = envChecks.value.find(c => !c.ok)
    if (bad) { Message.warning(`${bad.name}：${bad.detail || '未通过'}`); return }
    wz.step = 2
    return
  }
  if (!allPorts().length) { Message.warning('请至少设置一个 HTTP 或 HTTPS 端口'); return }
  if (!allPorts().includes(f.default)) { Message.warning('请选择默认入口端口'); return }
  if (portCheck.value && !portCheck.value.ok) { Message.warning(`${portCheck.value.name}：${portCheck.value.detail}`); return }
  wz.step = 3
}
const stepHint = computed(() => ['选择 OpenResty 的运行位置', '已安装的 OpenResty 会直接使用，不会重新安装', '设置用户站点可以使用的 Web 端口', wz.started ? '' : '确认后保存配置并部署'][wz.step])
const summary = computed(() => {
  const f = wz.f, c = wz.check, label = (list, v) => list.find(m => m.value === v)?.label || v
  let or = '部署时检测'
  if (c?.existing) or = '直接使用已安装的 ' + existingText(c.existing)
  else if (c && f.deploy === 'binary') or = `二进制安装（${label(mirrors.download, f.downloadMirror)}）`
  else if (c) or = `Docker 镜像 ${f.image}（${hasRegistryHost(f.image) ? '自带仓库' : label(mirrors.registry, f.registry)}）` + (c.docker ? '' : `；先从 ${label(mirrors.dockerSource, f.dockerSource)} 安装 Docker`)
  const rows = [
    { label: '运行位置', value: f.mode === 'vm' ? `入口虚拟机 ${vmLabel(wz.e, f.vmId)}` : `宿主机 ${wz.e.node.name}` },
    { label: 'OpenResty', value: or },
    { label: 'HTTP 端口', value: f.http.join('、') || '-' },
    { label: 'HTTPS 端口', value: f.https.join('、') || '-' },
    { label: '默认入口端口', value: f.default },
  ]
  if (f.stop.length) rows.push({ label: '停止容器', value: f.stop.join('、') + '（停用 Web 转发时恢复）' })
  if (conflicts.value.length) rows.push({ label: '接管端口转发', value: conflicts.value.map(r => `${r.port}「${r.remark || r.port}」`).join('、') })
  return rows
})

async function loadPubkey(e) {
  pubkeys[e.node.id] = null
  try { pubkeys[e.node.id] = (await api.post(`/api/nodes/${e.node.id}/web/pubkey`)).publicKey } catch { delete pubkeys[e.node.id] }
}
const installLog = ref(null)
let followLog = true
function onLogScroll() {
  const el = installLog.value
  if (el) followLog = el.scrollHeight - el.scrollTop - el.clientHeight <= 4
}
function appendLog(text) {
  const shouldFollow = followLog
  wz.log = (wz.log + text.replace(/\r\n/g, '\n').replace(/\r/g, '\n')).slice(-256 * 1024)
  nextTick(() => {
    if (shouldFollow && followLog && installLog.value) installLog.value.scrollTop = installLog.value.scrollHeight
  })
}
// 部署（失败后可直接重试，使用同样的参数）
async function deploy(takeOver) {
  wz.takeOver = takeOver
  resetRun()
  Object.assign(wz, { started: true, running: true })
  followLog = true
  try {
    await streamNDJSON(`/api/nodes/${wz.e.node.id}/web`, { ...webBody(), takeOver }, m => {
      if (m.log) appendLog(m.log)
      if (m.step) { wz.steps.push(m.step); appendLog(`${m.step.name}：${m.step.ok ? '完成' : m.step.error || '失败'}\n`) }
      if (m.error) wz.error = m.error
    })
  } catch (err) { wz.error = err.message }
  wz.running = false
  entries.value = await api.get('/api/web-entries')
}
function backToEdit() {
  resetRun()
  wz.step = 1
  runCheck()
}
</script>

<style scoped>
.entry-card { border: 1px solid var(--color-border-2); border-radius: 4px; padding: 16px 20px; }
.entry-sum { margin-top: 12px; }
.wz-loading { padding: 24px; text-align: center; color: var(--color-text-3); }
.checks.one { grid-template-columns: 1fr; }
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
.install-log { box-sizing: border-box; width: 100%; height: 280px; overflow: auto; margin: 12px 0 0; padding: 12px; background: #0d0d0d; color: #e5e5e5; border-radius: 4px; font: 12px/1.6 Consolas, monospace; white-space: pre-wrap; overflow-wrap: anywhere; overflow-anchor: none; }
.deploy-steps { line-height: 2; }
.deploy-steps .ok { color: rgb(var(--green-6)); }
.deploy-steps .no { color: rgb(var(--red-6)); }
.deploy-steps .err { margin-left: 22px; color: var(--color-text-2); font-size: 12px; line-height: 1.6; white-space: pre-wrap; word-break: break-all; }
.deploy-steps .wait { color: var(--color-text-3); }
</style>
