<template>
  <div class="gateway-publish-panel">
    <div class="share-panel-header">
      <div class="share-panel-header-row">
        <div class="share-panel-titlewrap">
          <h2 class="share-panel-title">{{ $t('agentEditor.gateway.title') }}</h2>
        </div>
      </div>
      <p class="share-panel-desc">{{ $t('agentEditor.gateway.desc') }}</p>
    </div>

    <t-loading :loading="initializing" show-overlay class="gateway-publish-body">
      <!-- 发布开关 -->
      <div class="gateway-publish-row gateway-publish-row--switch">
        <div class="gateway-publish-row-text">
          <div class="gateway-publish-row-title">{{ $t('agentEditor.gateway.publishLabel') }}</div>
          <div class="gateway-publish-row-desc">{{ $t('agentEditor.gateway.publishDesc') }}</div>
        </div>
        <t-switch v-model="form.published" :disabled="submitting" @change="markDirty" />
      </div>

      <!-- 已发布状态 -->
      <div v-if="agent?.published" class="gateway-publish-status">
        <t-tag size="small" variant="light" theme="success">
          {{ $t('agentEditor.gateway.versionTag', { version: agent.gateway_version ?? 0 }) }}
        </t-tag>
        <span v-if="agent.published_at" class="gateway-publish-status-meta">
          {{ $t('agentEditor.gateway.publishedAt') }} {{ formatDateTime(agent.published_at) }}
        </span>
        <t-tag v-if="agent.deprecation_sunset" size="small" variant="light" theme="warning">
          {{ $t('agentEditor.gateway.sunsetTag') }} {{ formatDateTime(agent.deprecation_sunset) }}
        </t-tag>
      </div>

      <template v-if="form.published">
        <!-- 调用范围 -->
        <div class="gateway-publish-row gateway-publish-row--stack">
          <div class="gateway-publish-row-title">{{ $t('agentEditor.gateway.scopeLabel') }}</div>
          <div class="gateway-publish-row-desc">{{ $t('agentEditor.gateway.scopeDesc') }}</div>
          <t-radio-group v-model="form.scope" variant="default-filled" size="small" :disabled="submitting" @change="markDirty">
            <t-radio-button value="tenant">{{ $t('agentEditor.gateway.scopeTenant') }}</t-radio-button>
            <t-radio-button value="org">{{ $t('agentEditor.gateway.scopeOrg') }}</t-radio-button>
            <t-radio-button value="private">{{ $t('agentEditor.gateway.scopePrivate') }}</t-radio-button>
          </t-radio-group>
        </div>

        <!-- 工具策略（M3-1） -->
        <div class="gateway-publish-row gateway-publish-row--stack">
          <div class="gateway-publish-row-title">{{ $t('agentEditor.gateway.policyLabel') }}</div>
          <div class="gateway-publish-row-desc">{{ $t('agentEditor.gateway.policyDesc') }}</div>
          <t-radio-group v-model="form.policy" variant="default-filled" size="small" :disabled="submitting" @change="markDirty">
            <t-radio-button value="all">{{ $t('agentEditor.gateway.policyAll') }}</t-radio-button>
            <t-radio-button value="client_only">{{ $t('agentEditor.gateway.policyClientOnly') }}</t-radio-button>
            <t-radio-button value="none">{{ $t('agentEditor.gateway.policyNone') }}</t-radio-button>
          </t-radio-group>
          <t-alert
            v-if="form.policy === 'client_only'" class="gateway-publish-alert" theme="warning"
            :message="$t('agentEditor.gateway.policyClientOnlyHint')" />
        </div>

        <!-- 弃用时间 -->
        <div class="gateway-publish-row gateway-publish-row--stack">
          <div class="gateway-publish-row-title">{{ $t('agentEditor.gateway.sunsetLabel') }}</div>
          <div class="gateway-publish-row-desc">{{ $t('agentEditor.gateway.sunsetDesc') }}</div>
          <t-date-picker
            v-model="form.sunset" class="gateway-publish-date" clearable allow-input
            enable-time-picker format="YYYY-MM-DD HH:mm" :disabled-date="disablePastDates"
            :placeholder="$t('agentEditor.gateway.sunsetPlaceholder')" :disabled="submitting" @change="markDirty" />
        </div>

        <!-- 调用端点 -->
        <div class="gateway-publish-row gateway-publish-row--stack">
          <div class="gateway-publish-row-title">{{ $t('agentEditor.gateway.endpointsTitle') }}</div>
          <div class="gateway-publish-row-desc">{{ $t('agentEditor.gateway.endpointsDesc') }}</div>
          <div class="gateway-endpoint-list">
            <div v-for="ep in endpoints" :key="ep.path" class="gateway-endpoint">
              <t-tag size="small" variant="outline" theme="primary" class="gateway-endpoint-method">{{ ep.method }}</t-tag>
              <code class="gateway-endpoint-path">{{ ep.path }}</code>
              <t-button shape="square" size="small" variant="text" class="gateway-endpoint-copy"
                :title="$t('common.copy')" :aria-label="$t('common.copy')" @click="copyEndpoint(ep)">
                <template #icon><t-icon name="file-copy" /></template>
              </t-button>
            </div>
          </div>
          <div class="gateway-publish-row-desc gateway-publish-key-hint">
            {{ canManageKeys ? $t('agentEditor.gateway.keyHint') : $t('agentEditor.gateway.keyHintNoPermission') }}
            <t-button v-if="canManageKeys" theme="primary" variant="text" size="small" @click="goApiKeys">
              {{ $t('agentEditor.gateway.goApiKeys') }}<t-icon name="chevron-right" size="14px" />
            </t-button>
          </div>
        </div>
      </template>
    </t-loading>

      <div class="gateway-publish-footer">
        <t-button theme="primary" :loading="submitting" :disabled="!dirty || submitting || initializing" @click="handleSave">
          {{ $t('common.save') }}
        </t-button>
      </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'
import { integrationSectionKey } from '@/config/settingsRoute'
import { copyWithToast } from '@/utils/clipboard'
import { publishAgent, type CustomAgent, type GatewayPublishScope, type GatewayToolPolicy } from '@/api/agent'

interface Props {
  agentId: string
  agent?: CustomAgent | null
}

const props = defineProps<Props>()
const emit = defineEmits<{
  (e: 'updated', agent: CustomAgent): void
}>()

const { t } = useI18n()
const router = useRouter()
const authStore = useAuthStore()

// Tenant API keys (the same set the gateway accepts) are managed by
// Owners on the "API Integration" tab — mirror the backend g.Owner()
// guard so non-owners get a hint instead of a dead-end jump.
const canManageKeys = computed(() => authStore.canAccessAllTenants || authStore.hasRole('owner'))

const initializing = ref(false)
const submitting = ref(false)
const dirty = ref(false)

const form = ref<{
  published: boolean;
  scope: GatewayPublishScope;
  policy: GatewayToolPolicy;
  sunset: string;
}>({ published: false, scope: 'tenant', policy: 'all', sunset: '' })

function hydrate(agent?: CustomAgent | null) {
  form.value.published = !!agent?.published
  const scope = (agent?.gateway_scope || 'tenant') as GatewayPublishScope
  form.value.scope = scope
  const policy = (agent?.config?.gateway_tool_policy || 'all') as GatewayToolPolicy
  form.value.policy = policy
  form.value.sunset = agent?.deprecation_sunset ? String(agent.deprecation_sunset) : ''
  dirty.value = false
}

watch(
  () => props.agent,
  (agent, prev) => {
    if (!dirty.value || agent?.id !== prev?.id) hydrate(agent)
  },
  { immediate: true },
)

const markDirty = () => {
  dirty.value = true
}

const endpoints = computed(() => [
  { method: 'GET', path: '/v1/models' },
  { method: 'POST', path: '/v1/chat/completions' },
  { method: 'POST', path: '/v1/messages' },
])

function disablePastDates(d: Date) {
  const startOfToday = new Date()
  startOfToday.setHours(0, 0, 0, 0)
  return d.getTime() < startOfToday.getTime()
}

function formatDateTime(value?: string) {
  if (!value) return '—'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  return d.toLocaleString()
}

async function copyEndpoint(ep: { method: string; path: string }) {
  await copyWithToast(`${window.location.origin}${ep.path}`, 'agentEditor.gateway.copied')
}

function goApiKeys() {
  // Key management lives on the "API Integration" tab (integration-api),
  // the same tenant API keys the gateway accepts.
  void router.push({ path: '/platform/settings', query: { section: integrationSectionKey('api') } })
}

function normalizeSunset(raw: string): string | null | undefined {
  const trimmed = (raw || '').trim()
  if (!trimmed) {
    // Absent keeps the stored value only when nothing was stored before.
    return props.agent?.deprecation_sunset ? null : undefined
  }
  const d = new Date(trimmed)
  if (Number.isNaN(d.getTime())) return undefined
  return d.toISOString()
}

async function handleSave() {
  if (!props.agentId || submitting.value) return
  submitting.value = true
  try {
    const payload: Record<string, unknown> = { published: form.value.published }
    if (form.value.published) {
      payload.published_scope = form.value.scope
      payload.gateway_tool_policy = form.value.policy
      const sunset = normalizeSunset(form.value.sunset)
      if (sunset !== undefined) payload.deprecation_sunset = sunset
    }
    const result = await publishAgent(props.agentId, payload)
    const updated = result?.data
    if (!updated?.id) throw new Error(result?.message || t('agent.messages.saveFailed'))
    dirty.value = false
    hydrate(updated)
    emit('updated', updated)
    MessagePlugin.success(
      updated.published ? t('agentEditor.gateway.publishSuccess') : t('agentEditor.gateway.unpublishSuccess'),
    )
  } catch (e: any) {
    MessagePlugin.error(e?.message || t('agent.messages.saveFailed'))
  } finally {
    submitting.value = false
  }
}
</script>

<style scoped lang="less">
.gateway-publish-panel {
  display: flex;
  flex-direction: column;
  gap: 16px;
  height: 100%;
  overflow-y: auto;
}

.gateway-publish-body {
  display: flex;
  flex-direction: column;
  gap: 20px;
  min-height: 120px;
}

.gateway-publish-row {
  display: flex;
  gap: 16px;
  align-items: flex-start;
  padding: 16px;
  border-radius: 9px;
  background: var(--td-bg-color-secondarycontainer, var(--td-bg-color-container));
}

.gateway-publish-row--stack {
  flex-direction: column;
  gap: 8px;
}

.gateway-publish-row-title {
  font-weight: 600;
  color: var(--td-text-color-primary);
}

.gateway-publish-row-desc {
  font-size: 12px;
  color: var(--td-text-color-secondary);
  line-height: 1.6;
}

.gateway-publish-status {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}

.gateway-publish-status-meta {
  font-size: 12px;
  color: var(--td-text-color-secondary);
}

.gateway-publish-alert {
  width: 100%;
}

.gateway-publish-date {
  width: 320px;
  max-width: 100%;
}

.gateway-endpoint-list {
  display: flex;
  flex-direction: column;
  gap: 6px;
  width: 100%;
}

.gateway-endpoint {
  display: flex;
  align-items: center;
  gap: 8px;
}

.gateway-endpoint-method {
  min-width: 44px;
  justify-content: center;
}

.gateway-endpoint-path {
  flex: 1;
  font-size: 12px;
  padding: 4px 8px;
  border-radius: 6px;
  background: var(--td-bg-color-secondarycontainer);
  color: var(--td-text-color-primary);
  word-break: break-all;
}

.gateway-publish-key-hint {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
}

.gateway-publish-footer {
  display: flex;
  justify-content: flex-end;
}
</style>
