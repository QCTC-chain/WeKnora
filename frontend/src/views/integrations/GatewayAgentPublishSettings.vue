<template>
  <div class="gateway-agents-settings">
    <!-- 调用端点与凭据 -->
    <div class="gateway-agents-card">
      <div class="gateway-agents-card-title">{{ $t('agentEditor.gateway.endpointsTitle') }}</div>
      <div class="gateway-agents-card-desc">{{ $t('integrations.gateway.endpointsDesc') }}</div>
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
      <div class="gateway-agents-card-desc gateway-agents-key-hint">
        {{ canManageKeys ? $t('agentEditor.gateway.keyHint') : $t('agentEditor.gateway.keyHintNoPermission') }}
        <t-button v-if="canManageKeys" theme="primary" variant="text" size="small" @click="goApiKeys">
          {{ $t('agentEditor.gateway.goApiKeys') }}<t-icon name="chevron-right" size="14px" />
        </t-button>
        <t-button theme="primary" variant="text" size="small" @click="openDocs">
          {{ $t('integrations.gateway.docLink') }}<t-icon name="link" size="14px" />
        </t-button>
      </div>
    </div>

    <!-- 智能体发布列表 -->
    <t-loading :loading="loading" show-overlay>
      <div class="gateway-agents-list">
        <div v-if="!loading && manageableRows.length === 0 && otherPublished.length === 0" class="gateway-agents-empty">
          <t-icon name="cloud" class="gateway-agents-empty-icon" />
          <p>{{ $t('integrations.gateway.empty') }}</p>
        </div>

        <div v-for="row in manageableRows" :key="row.agent.id" class="gateway-agent-card" :class="{ 'gateway-agent-card--published': formOf(row).published }">
          <div class="gateway-agent-head">
            <div class="gateway-agent-info">
              <div class="gateway-agent-name">
                {{ row.agent.name }}
                <t-tag v-if="formOf(row).published" size="small" variant="light" theme="success">
                  {{ $t('agentEditor.gateway.versionTag', { version: row.agent.gateway_version ?? 0 }) }}
                </t-tag>
                <t-tag v-if="row.agent.deprecation_sunset" size="small" variant="light" theme="warning">
                  {{ $t('agentEditor.gateway.sunsetTag') }} {{ formatDateTime(row.agent.deprecation_sunset) }}
                </t-tag>
              </div>
              <div v-if="row.agent.description" class="gateway-agent-desc">{{ row.agent.description }}</div>
            </div>
            <t-switch :value="formOf(row).published" :disabled="row.submitting" @change="(v: boolean) => onPublishToggle(row, v)" />
          </div>

          <template v-if="formOf(row).published">
            <div class="gateway-agent-fields">
              <div class="gateway-agent-field">
                <span class="gateway-agent-field-label">{{ $t('agentEditor.gateway.scopeLabel') }}</span>
                <t-radio-group v-model="formOf(row).scope" variant="default-filled" size="small" :disabled="row.submitting" @change="markDirty(row)">
                  <t-radio-button value="tenant">{{ $t('agentEditor.gateway.scopeTenant') }}</t-radio-button>
                  <t-radio-button value="org">{{ $t('agentEditor.gateway.scopeOrg') }}</t-radio-button>
                  <t-radio-button value="private">{{ $t('agentEditor.gateway.scopePrivate') }}</t-radio-button>
                </t-radio-group>
              </div>
              <div class="gateway-agent-field">
                <span class="gateway-agent-field-label">{{ $t('agentEditor.gateway.policyLabel') }}</span>
                <t-radio-group v-model="formOf(row).policy" variant="default-filled" size="small" :disabled="row.submitting" @change="markDirty(row)">
                  <t-radio-button value="all">{{ $t('agentEditor.gateway.policyAll') }}</t-radio-button>
                  <t-radio-button value="client_only">{{ $t('agentEditor.gateway.policyClientOnly') }}</t-radio-button>
                  <t-radio-button value="none">{{ $t('agentEditor.gateway.policyNone') }}</t-radio-button>
                </t-radio-group>
              </div>
              <div class="gateway-agent-field">
                <span class="gateway-agent-field-label">{{ $t('agentEditor.gateway.sunsetLabel') }}</span>
                <t-date-picker
                  v-model="formOf(row).sunset" class="gateway-agent-date" clearable allow-input
                  enable-time-picker format="YYYY-MM-DD HH:mm" :disabled-date="disablePastDates"
                  :placeholder="$t('agentEditor.gateway.sunsetPlaceholder')" :disabled="row.submitting" @change="markDirty(row)" />
              </div>
            </div>
            <t-alert
              v-if="formOf(row).policy === 'client_only'" class="gateway-agent-alert" theme="warning"
              :message="$t('agentEditor.gateway.policyClientOnlyHint')" />
            <div class="gateway-agent-actions">
              <t-button theme="primary" size="small" :loading="row.submitting"
                :disabled="!row.dirty || row.submitting" @click="saveRow(row)">
                {{ $t('common.save') }}
              </t-button>
            </div>
          </template>
        </div>

        <!-- 他人发布、只读展示 -->
        <div v-if="otherPublished.length > 0" class="gateway-agents-others">
          <div class="gateway-agents-others-title">{{ $t('integrations.gateway.othersTitle') }}</div>
          <div v-for="agent in otherPublished" :key="agent.id" class="gateway-agent-card gateway-agent-card--readonly">
            <div class="gateway-agent-head">
              <div class="gateway-agent-info">
                <div class="gateway-agent-name">
                  {{ agent.name }}
                  <t-tag size="small" variant="light" theme="success">
                    {{ $t('agentEditor.gateway.versionTag', { version: agent.gateway_version ?? 0 }) }}
                  </t-tag>
                </div>
                <div v-if="agent.description" class="gateway-agent-desc">{{ agent.description }}</div>
              </div>
              <span class="gateway-agent-creator">{{ agent.creator_name || '' }}</span>
            </div>
          </div>
        </div>
      </div>
    </t-loading>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { copyWithToast } from '@/utils/clipboard'
import { docsUrl } from '@/utils/docsUrl'
import { useAuthStore } from '@/stores/auth'
import { integrationSectionKey } from '@/config/settingsRoute'
import {
  listAgents, publishAgent,
  type CustomAgent, type GatewayPublishScope, type GatewayToolPolicy,
} from '@/api/agent'

interface RowForm {
  published: boolean;
  scope: GatewayPublishScope;
  policy: GatewayToolPolicy;
  sunset: string;
}

interface AgentRow {
  agent: CustomAgent;
  form: RowForm;
  dirty: boolean;
  submitting: boolean;
}

const { t } = useI18n()
const router = useRouter()
const authStore = useAuthStore()

// The gateway accepts the tenant's API keys (Owner-managed on the
// "API Integration" tab, backend g.Owner()); mirror that here so
// non-owners get a hint instead of a jump that bounces off the
// owner-only tab.
const canManageKeys = computed(() => authStore.canAccessAllTenants || authStore.hasRole('owner'))

const loading = ref(false)
const rows = reactive<Record<string, AgentRow>>({})

const endpoints = [
  { method: 'GET', path: '/v1/models' },
  { method: 'POST', path: '/v1/chat/completions' },
  { method: 'POST', path: '/v1/messages' },
]

// canManage mirrors AgentList's client-side guard for the server's
// OwnedAgentOrAdmin matrix: creator (contributor+) or Admin+.
function canManage(agent: CustomAgent): boolean {
  if (agent.is_builtin) return false
  const userId = authStore.user?.id || ''
  const creatorId = agent.created_by || ''
  if (creatorId && userId && creatorId === userId && authStore.hasRole('contributor')) return true
  return authStore.hasRole('admin')
}

const manageableRows = computed(() =>
  Object.values(rows).filter((row) => canManage(row.agent)))
const otherPublished = computed(() =>
  Object.values(rows)
    .map((row) => row.agent)
    .filter((agent) => !canManage(agent) && agent.published))

function formOf(row: AgentRow): RowForm {
  return row.form
}

function hydrate(agent: CustomAgent): RowForm {
  return {
    published: !!agent.published,
    scope: (agent.gateway_scope || 'tenant') as GatewayPublishScope,
    policy: (agent.config?.gateway_tool_policy || 'all') as GatewayToolPolicy,
    sunset: agent.deprecation_sunset ? String(agent.deprecation_sunset) : '',
  }
}

async function load() {
  loading.value = true
  try {
    const result = await listAgents()
    const agents = (result?.data || []).filter((agent) => !agent.is_builtin)
    for (const agent of agents) {
      const prev = rows[agent.id]
      const keepForm = prev && prev.dirty ? prev.form : hydrate(agent)
      rows[agent.id] = { agent, form: keepForm, dirty: prev?.dirty ?? false, submitting: false }
    }
    // Drop rows for agents that disappeared.
    for (const id of Object.keys(rows)) {
      if (!agents.some((agent) => agent.id === id)) delete rows[id]
    }
  } catch (e: any) {
    MessagePlugin.error(e?.message || t('agent.messages.loadFailed'))
  } finally {
    loading.value = false
  }
}

onMounted(load)

const markDirty = (row: AgentRow) => {
  row.dirty = true
}

async function onPublishToggle(row: AgentRow, next: boolean) {
  // The switch feeds the same save pipeline: flip the form, then persist
  // immediately (publish/unpublish is a governance PATCH, never a
  // config update — safe to apply directly).
  row.form.published = next
  row.dirty = true
  await saveRow(row)
}

function normalizeSunset(row: AgentRow, raw: string): string | null | undefined {
  const trimmed = (raw || '').trim()
  if (!trimmed) {
    // Absent keeps the stored value only when nothing was stored before.
    return row.agent.deprecation_sunset ? null : undefined
  }
  const d = new Date(trimmed)
  if (Number.isNaN(d.getTime())) return undefined
  return d.toISOString()
}

async function saveRow(row: AgentRow) {
  if (row.submitting) return
  row.submitting = true
  try {
    const payload: Record<string, unknown> = { published: row.form.published }
    if (row.form.published) {
      payload.published_scope = row.form.scope
      payload.gateway_tool_policy = row.form.policy
      const sunset = normalizeSunset(row, row.form.sunset)
      if (sunset !== undefined) payload.deprecation_sunset = sunset
    }
    const result = await publishAgent(row.agent.id, payload)
    const updated = result?.data
    if (!updated?.id) throw new Error(result?.message || t('agent.messages.saveFailed'))
    row.agent = updated
    row.form = hydrate(updated)
    row.dirty = false
    MessagePlugin.success(
      updated.published ? t('agentEditor.gateway.publishSuccess') : t('agentEditor.gateway.unpublishSuccess'),
    )
  } catch (e: any) {
    // Roll the switch back visually; the authoritative state comes from
    // the backend on the next load.
    row.form.published = !!row.agent.published
    row.dirty = false
    MessagePlugin.error(e?.message || t('agent.messages.saveFailed'))
  } finally {
    row.submitting = false
  }
}

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
  // Key management lives on the "API Integration" tab; those keys are
  // exactly what the gateway accepts (chat capability or full access).
  void router.push({ path: '/platform/settings', query: { section: integrationSectionKey('api') } })
}

function openDocs() {
  window.open(docsUrl('gatewayApi'), '_blank', 'noopener noreferrer')
}
</script>

<style scoped lang="less">
.gateway-agents-settings {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.gateway-agents-card {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 16px;
  border-radius: 9px;
  background: var(--td-bg-color-secondarycontainer, var(--td-bg-color-container));
}

.gateway-agents-card-title {
  font-weight: 600;
  color: var(--td-text-color-primary);
}

.gateway-agents-card-desc {
  font-size: 12px;
  color: var(--td-text-color-secondary);
  line-height: 1.6;
}

.gateway-agents-key-hint {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
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
  background: var(--td-bg-color-container);
  color: var(--td-text-color-primary);
  word-break: break-all;
}

.gateway-agents-list {
  display: flex;
  flex-direction: column;
  gap: 12px;
  min-height: 80px;
}

.gateway-agents-empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  padding: 32px 16px;
  color: var(--td-text-color-secondary);
  font-size: 13px;
}

.gateway-agents-empty-icon {
  font-size: 32px;
  color: var(--td-text-color-placeholder);
}

.gateway-agent-card {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 16px;
  border-radius: 9px;
  border: 1px solid var(--td-component-border);
  background: var(--td-bg-color-secondarycontainer, var(--td-bg-color-container));

  &--published {
    border-color: var(--td-success-color-3);
  }

  &--readonly {
    border-style: dashed;
  }
}

.gateway-agent-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
}

.gateway-agent-info {
  min-width: 0;
}

.gateway-agent-name {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
  font-weight: 600;
  color: var(--td-text-color-primary);
}

.gateway-agent-desc {
  margin-top: 4px;
  font-size: 12px;
  color: var(--td-text-color-secondary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.gateway-agent-creator {
  flex-shrink: 0;
  font-size: 12px;
  color: var(--td-text-color-secondary);
}

.gateway-agent-fields {
  display: flex;
  flex-wrap: wrap;
  gap: 16px;
}

.gateway-agent-field {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.gateway-agent-field-label {
  font-size: 12px;
  font-weight: 600;
  color: var(--td-text-color-primary);
}

.gateway-agent-date {
  width: 280px;
  max-width: 100%;
}

.gateway-agent-alert {
  width: 100%;
}

.gateway-agent-actions {
  display: flex;
  justify-content: flex-end;
}

.gateway-agents-others {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.gateway-agents-others-title {
  font-size: 12px;
  font-weight: 600;
  color: var(--td-text-color-secondary);
}
</style>
