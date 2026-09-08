<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  NAlert, NButton, NCard, NForm, NFormItem, NIcon, NInput, NInputNumber,
  NSelect, NSpace, NSwitch, useMessage,
} from 'naive-ui'
import { ArrowBackOutline, RocketOutline } from '@vicons/ionicons5'
import { createApp, type AppEntity, type PortMapping } from '../../api/apps'
import { listHosts } from '../../api/hosts'
import { listCredentials } from '../../api/vault'
import { useWorkspaceStore } from '../../stores/workspace'

defineOptions({ name: 'AppCreate' })

const router = useRouter()
const message = useMessage()
const workspace = useWorkspaceStore()

interface HostOption {
  id: string
  hostname: string
  status: string
  os: string
}

interface VaultOption {
  id: string
  name: string
  type: string
}

const hosts = ref<HostOption[]>([])
const vaults = ref<VaultOption[]>([])
const creating = ref(false)

const form = reactive({
  name: '',
  host_id: '',
  repo_url: '',
  branch: 'main',
  auth_vault_id: '',
  auto_deploy: true,
  build_type: 'dockerfile' as 'dockerfile' | 'compose',
  dockerfile: 'Dockerfile',
  build_context: '.',
  build_timeout_sec: 900,
  env_vars_text: '',
  ports: [{ host: 8080, container: 8080 }] as PortMapping[],
  volumes_text: '',
  healthcheck_url: '',
  container_name: '',
})

const hostOptions = computed(() =>
  hosts.value.map((h) => ({
    label: `${h.hostname} (${h.os}${h.status !== 'online' ? ' · 离线' : ''})`,
    value: h.id,
    disabled: h.status !== 'online',
  })),
)

const vaultOptions = computed(() => [
  { label: '不使用凭据（公开仓库）', value: '' },
  ...vaults.value.map((v) => ({ label: `${v.name} (${v.type})`, value: v.id })),
])

const parsedEnvVars = computed<Record<string, string> | undefined>(() => {
  const text = form.env_vars_text.trim()
  if (!text) return undefined
  const vars: Record<string, string> = {}
  for (const line of text.split('\n')) {
    const trimmed = line.trim()
    if (!trimmed || trimmed.startsWith('#')) continue
    const eq = trimmed.indexOf('=')
    if (eq <= 0) continue
    vars[trimmed.slice(0, eq).trim()] = trimmed.slice(eq + 1).trim()
  }
  return Object.keys(vars).length > 0 ? vars : undefined
})

const parsedVolumes = computed<string[] | undefined>(() => {
  const text = form.volumes_text.trim()
  if (!text) return undefined
  const list = text
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l.length > 0)
  return list.length > 0 ? list : undefined
})

async function loadData() {
  try {
    const res = await listHosts()
    hosts.value = ((res as { data: HostOption[] }).data ?? res) as HostOption[]
  } catch {
    hosts.value = []
  }
  try {
    vaults.value = (await listCredentials()) as VaultOption[]
  } catch {
    vaults.value = []
  }
}

async function submit() {
  if (!form.name.trim()) {
    message.warning('请填写应用名称')
    return
  }
  if (!form.host_id) {
    message.warning('请选择目标主机')
    return
  }
  if (!form.repo_url.trim()) {
    message.warning('请填写 Git 仓库地址')
    return
  }
  if (!/^https?:\/\//.test(form.repo_url.trim())) {
    message.warning('仓库地址必须是 http(s):// 开头的 URL')
    return
  }
  creating.value = true
  try {
    const app = (await createApp({
      name: form.name.trim(),
      host_id: form.host_id,
      repo_url: form.repo_url.trim(),
      branch: form.branch.trim() || 'main',
      auth_vault_id: form.auth_vault_id || undefined,
      auto_deploy: form.auto_deploy,
      build_type: form.build_type,
      dockerfile: form.dockerfile.trim() || (form.build_type === 'compose' ? 'compose.yaml' : 'Dockerfile'),
      build_context: form.build_context.trim() || '.',
      build_timeout_sec: Number(form.build_timeout_sec) || 900,
      env_vars: parsedEnvVars.value,
      ports: form.ports.filter((p) => p.host > 0 && p.container > 0),
      volumes: parsedVolumes.value,
      healthcheck_url: form.healthcheck_url.trim(),
      container_name: form.container_name.trim() || undefined,
    })) as AppEntity
    message.success(`应用「${app.name}」创建成功`)
    workspace.openTab({
      key: `/apps/${app.id}`,
      title: `应用 ${app.name}`,
      path: `/apps/${app.id}`,
      viewName: 'AppDetail',
    })
    router.replace(`/apps/${app.id}`)
  } catch (e: any) {
    message.error(e.message || '创建失败')
  } finally {
    creating.value = false
  }
}

function addPort() {
  form.ports.push({ host: 0, container: 0 })
}

function removePort(idx: number) {
  form.ports.splice(idx, 1)
}

onMounted(loadData)
</script>

<template>
  <div class="app-create-view">
    <NSpace align="center" style="margin-bottom: 12px">
      <NButton quaternary @click="router.push('/apps')">
        <template #icon>
          <NIcon><ArrowBackOutline /></NIcon>
        </template>
        返回应用列表
      </NButton>
    </NSpace>

    <NCard title="链接 Git 应用" class="create-card">
      <template #header>
        <NSpace align="center" :size="6">
          <NIcon><RocketOutline /></NIcon>
          <span>链接 Git 应用</span>
        </NSpace>
      </template>

      <NSpace vertical size="large">
        <NAlert type="info" :show-icon="true">
          将 Git 仓库链接为受管应用：推送代码到监听分支即自动构建并滚动发布，
          也可在应用详情页随时手动重新部署或回滚。
        </NAlert>

        <NForm label-placement="top">
          <NFormItem label="应用名称" required>
            <NInput v-model:value="form.name" placeholder="如 my-web-api" />
          </NFormItem>

          <NFormItem label="目标主机" required>
            <NSelect
              v-model:value="form.host_id"
              :options="hostOptions"
              placeholder="选择部署到的受管主机（需在线）"
              filterable
            />
          </NFormItem>

          <NFormItem label="Git 仓库地址" required>
            <NInput
              v-model:value="form.repo_url"
              placeholder="https://github.com/yourname/your-repo.git"
            />
          </NFormItem>

          <NSpace>
            <NFormItem label="监听分支" style="flex: 1">
              <NInput v-model:value="form.branch" placeholder="main" />
            </NFormItem>
            <NFormItem label="容器名（可选）" style="flex: 1">
              <NInput v-model:value="form.container_name" placeholder="留空自动生成" />
            </NFormItem>
          </NSpace>

          <NFormItem label="Git 凭据（私有仓库需要）">
            <NSelect
              v-model:value="form.auth_vault_id"
              :options="vaultOptions"
              placeholder="从凭据金库选择 Access Token"
            />
          </NFormItem>

          <NFormItem label="构建部署模式">
            <NRadioGroup v-model:value="form.build_type">
              <NSpace>
                <NRadio value="dockerfile">Dockerfile 单容器镜像构建</NRadio>
                <NRadio value="compose">Docker Compose 复合微服务栈</NRadio>
              </NSpace>
            </NRadioGroup>
          </NFormItem>

          <NFormItem :label="form.build_type === 'compose' ? 'Compose 文件路径' : 'Dockerfile 路径'">
            <NInput
              v-model:value="form.dockerfile"
              :placeholder="form.build_type === 'compose' ? 'compose.yaml' : 'Dockerfile'"
            />
          </NFormItem>

          <NFormItem label="健康检查 URL（可选，推荐）">
            <NInput
              v-model:value="form.healthcheck_url"
              placeholder="http://127.0.0.1:8080/healthz（探活失败将自动保留旧版本）"
            />
          </NFormItem>

          <NFormItem label="端口映射">
            <NSpace vertical style="width: 100%">
              <NSpace v-for="(p, idx) in form.ports" :key="idx" align="center">
                <NInputNumber v-model:value="p.host" placeholder="宿主端口" style="width: 140px" />
                <span>-></span>
                <NInputNumber v-model:value="p.container" placeholder="容器端口" style="width: 140px" />
                <NButton quaternary type="error" size="small" @click="removePort(idx)">移除</NButton>
              </NSpace>
              <NButton dashed size="small" @click="addPort">添加端口映射</NButton>
            </NSpace>
          </NFormItem>

          <NFormItem label="环境变量（每行 KEY=VALUE，值支持 {{ vault:id:secret }} 引用）">
            <NInput
              v-model:value="form.env_vars_text"
              type="textarea"
              :rows="4"
              placeholder="DATABASE_URL=postgres://...&#10;API_KEY={{ vault:cred123:secret }}"
            />
          </NFormItem>

          <NFormItem label="挂载卷（每行 宿主路径:容器路径）">
            <NInput
              v-model:value="form.volumes_text"
              type="textarea"
              :rows="2"
              placeholder="/opt/apps/demo/data:/app/data"
            />
          </NFormItem>

          <NFormItem label="推送代码自动部署（Webhook）">
            <NSpace align="center">
              <NSwitch v-model:value="form.auto_deploy" />
              <span v-if="form.auto_deploy" class="hint">创建后请在应用详情页复制 Webhook 地址配置到代码仓库</span>
            </NSpace>
          </NFormItem>
        </NForm>

        <NSpace justify="end">
          <NButton @click="router.push('/apps')">取消</NButton>
          <NButton type="primary" :loading="creating" @click="submit">
            创建并链接 Git
          </NButton>
        </NSpace>
      </NSpace>
    </NCard>
  </div>
</template>

<style scoped lang="scss">
.app-create-view {
  height: 100%;
  min-height: 0;
  overflow-y: auto;
  overscroll-behavior: contain;
}

.create-card {
  max-width: 860px;
}

.hint {
  font-size: 12px;
  color: var(--n-text-color-disabled, #999);
}
</style>