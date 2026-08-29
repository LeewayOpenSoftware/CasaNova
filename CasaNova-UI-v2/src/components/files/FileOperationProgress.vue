<script setup lang="ts">
import { ref, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import AppIcon from '@/components/ui/AppIcon.vue'
import MorphIcon from '@/components/ui/MorphIcon.vue'
import { useFilesStore, formatBytes, type FileOperation } from '@/stores/files'

const { t } = useI18n()
const files = useFilesStore()

const collapsed = ref(false)

const hasCompleted = computed(() =>
  files.operations.some((o) => o.status === 'done' || o.status === 'error'),
)

function toggleCollapse() {
  collapsed.value = !collapsed.value
}

function elapsed(op: FileOperation): string {
  const secs = Math.floor((Date.now() - op.startedAt) / 1000)
  if (secs < 60) return `${secs}s`
  const mins = Math.floor(secs / 60)
  const rem = secs % 60
  return `${mins}m ${rem}s`
}

function operationLabel(op: FileOperation): string {
  if (op.status === 'done') return t('files.operations.done')
  if (op.status === 'error') {
    if (op.error === '__canceled__') return t('files.operations.canceled')
    return op.error ?? t('files.operations.error')
  }
  switch (op.type) {
    case 'upload':
      return t('files.operations.uploading', { count: op.totalFiles }, op.totalFiles)
    case 'download':
      return t('files.operations.downloading', { count: op.totalFiles }, op.totalFiles)
    case 'copy':
      return t('files.operations.copying', { count: op.totalFiles }, op.totalFiles)
    case 'move':
      return t('files.operations.moving', { count: op.totalFiles }, op.totalFiles)
    case 'delete':
      return t('files.operations.deleting', { count: op.totalFiles }, op.totalFiles)
  }
}

function progressText(op: FileOperation): string {
  if (op.progress < 0) return ''
  return `${op.progress}%`
}

function bytesText(op: FileOperation): string {
  if (op.type !== 'upload' && op.type !== 'download') return ''
  if (!op.totalBytes) {
    // Streaming archives have no content-length — show bytes so far.
    return op.loadedBytes ? formatBytes(op.loadedBytes) : ''
  }
  return `${formatBytes(op.loadedBytes ?? 0)} / ${formatBytes(op.totalBytes)}`
}

function fileCountText(op: FileOperation): string {
  if (op.totalFiles <= 1) return ''
  return t('files.operations.of', { completed: op.completedFiles, total: op.totalFiles })
}
</script>

<template>
  <Transition name="op-panel">
    <div
      v-if="files.operations.length > 0"
      class="op-panel"
    >
      <!-- Header — keyboard-operable collapse toggle (was a bare click div) -->
      <div
        class="op-header"
        role="button"
        tabindex="0"
        :aria-expanded="!collapsed"
        @click="toggleCollapse"
        @keydown.enter.prevent="toggleCollapse"
        @keydown.space.prevent="toggleCollapse"
      >
        <span class="op-title">{{ t('files.operations.title') }}</span>
        <div class="op-header-actions">
          <button
            v-if="hasCompleted"
            class="op-clear-btn"
            type="button"
            @click.stop="files.clearCompleted()"
          >
            {{ t('files.operations.clear') }}
          </button>
          <MorphIcon :name="collapsed ? 'ChevronUp' : 'ChevronDown'" :size="14" />
        </div>
      </div>

      <!-- Body -->
      <Transition name="op-body">
        <div v-if="!collapsed" class="op-body">
          <TransitionGroup name="op-item" tag="div">
            <div
              v-for="op in files.operations"
              :key="op.id"
              class="op-item"
              :class="{
                'op-item--done': op.status === 'done',
                'op-item--error': op.status === 'error',
              }"
            >
              <div class="op-row">
                <!-- Icon -->
                <div class="op-icon">
                  <AppIcon v-if="op.status === 'done'" name="Check" :size="16" class="text-emerald-500" />
                  <AppIcon v-else-if="op.status === 'error'" name="XCircle" :size="16" class="text-red-500" />
                  <AppIcon v-else-if="op.type === 'upload'" name="Upload" :size="16" />
                  <AppIcon v-else-if="op.type === 'download'" name="Download" :size="16" />
                  <AppIcon v-else-if="op.type === 'copy'" name="Copy" :size="16" />
                  <AppIcon v-else-if="op.type === 'move'" name="ArrowRight" :size="16" />
                  <AppIcon v-else-if="op.type === 'delete'" name="Trash2" :size="16" />
                </div>

                <!-- Label + percentage -->
                <div class="op-info">
                  <span class="op-label">{{ operationLabel(op) }}</span>
                  <span v-if="op.status === 'running' && op.progress >= 0" class="op-pct">
                    {{ progressText(op) }}
                  </span>
                </div>

                <!-- Cancel (running uploads & downloads) — per-operation -->
                <button
                  v-if="op.status === 'running' && (op.type === 'upload' || op.type === 'download')"
                  class="op-clear-btn"
                  type="button"
                  :title="t('files.operations.cancel')"
                  @click.stop="files.cancelOperation(op.id)"
                >
                  {{ t('files.operations.cancel') }}
                </button>
              </div>

              <!-- Progress bar (running only) -->
              <div v-if="op.status === 'running' && op.progress >= 0" class="op-bar-track">
                <div
                  class="op-bar-fill op-bar-fill--running"
                  :style="{ width: op.progress + '%' }"
                />
              </div>
              <!-- Indeterminate bar -->
              <div v-else-if="op.status === 'running' && op.progress < 0" class="op-bar-track">
                <div class="op-bar-fill op-bar-fill--indeterminate" />
              </div>
              <!-- Done bar -->
              <div v-else-if="op.status === 'done'" class="op-bar-track">
                <div class="op-bar-fill op-bar-fill--done" style="width: 100%" />
              </div>
              <!-- Error bar -->
              <div v-else-if="op.status === 'error'" class="op-bar-track">
                <div class="op-bar-fill op-bar-fill--error" style="width: 100%" />
              </div>

              <!-- Detail line: current file, bytes, elapsed -->
              <div v-if="op.status === 'running'" class="op-detail">
                <span v-if="op.currentFile" class="op-current-file">
                  {{ op.currentFile }}
                  <template v-if="fileCountText(op)">
                    ({{ fileCountText(op) }})
                  </template>
                </span>
                <span v-if="bytesText(op)" class="op-bytes">{{ bytesText(op) }}</span>
                <span class="op-elapsed">{{ elapsed(op) }}</span>
              </div>

              <!-- Error message (skip the internal cancel sentinel — the
                   label row already says "canceled" via i18n) -->
              <div v-if="op.status === 'error' && op.error && op.error !== '__canceled__'" class="op-error-msg">
                {{ op.error }}
              </div>
            </div>
          </TransitionGroup>
        </div>
      </Transition>
    </div>
  </Transition>
</template>

<style scoped>
.op-panel {
  position: absolute;
  bottom: 8px;
  right: 8px;
  width: 340px;
  max-height: 400px;
  border-radius: 10px;
  border: 1px solid var(--cn-border, rgba(255, 255, 255, 0.08));
  background: var(--cn-surface, rgba(30, 30, 30, 0.95));
  box-shadow: 0 8px 32px rgba(0, 0, 0, 0.25);
  z-index: 50;
  overflow: hidden;
  display: flex;
  flex-direction: column;
}

.op-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 8px 12px;
  cursor: pointer;
  user-select: none;
  border-bottom: 1px solid var(--cn-border, rgba(255, 255, 255, 0.08));
  color: var(--cn-text, #e0e0e0);
  font-size: 13px;
  font-weight: 600;
}

.op-header-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--cn-text-muted, #888);
}

.op-clear-btn {
  font-size: 11px;
  padding: 2px 8px;
  border-radius: 4px;
  border: none;
  background: var(--cn-surface-hover, rgba(255, 255, 255, 0.06));
  color: var(--cn-text-muted, #888);
  cursor: pointer;
  transition: background 0.15s, color 0.15s;
}

.op-clear-btn:hover {
  background: var(--cn-surface-active, rgba(255, 255, 255, 0.12));
  color: var(--cn-text, #e0e0e0);
}

.op-title {
  white-space: nowrap;
}

.op-body {
  overflow-y: auto;
  max-height: 340px;
}

.op-item {
  padding: 8px 12px;
  border-bottom: 1px solid var(--cn-border, rgba(255, 255, 255, 0.05));
}

.op-item:last-child {
  border-bottom: none;
}

.op-row {
  display: flex;
  align-items: center;
  gap: 8px;
}

.op-icon {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  color: var(--cn-text-muted, #888);
}

.op-item--done .op-icon {
  color: #22c55e;
}

.op-item--error .op-icon {
  color: #ef4444;
}

.op-info {
  flex: 1;
  display: flex;
  align-items: center;
  justify-content: space-between;
  min-width: 0;
}

.op-label {
  font-size: 12px;
  color: var(--cn-text, #e0e0e0);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.op-item--done .op-label {
  color: #22c55e;
}

.op-item--error .op-label {
  color: #ef4444;
}

.op-pct {
  font-size: 11px;
  color: var(--cn-text-muted, #888);
  flex-shrink: 0;
  margin-left: 8px;
}

/* Progress bar track */
.op-bar-track {
  margin-top: 6px;
  height: 4px;
  border-radius: 2px;
  background: var(--cn-surface-hover, rgba(255, 255, 255, 0.06));
  overflow: hidden;
}

.op-bar-fill {
  height: 100%;
  border-radius: 2px;
  transition: width 0.3s ease;
}

.op-bar-fill--running {
  background: var(--cn-accent);
}

.op-bar-fill--done {
  background: var(--cn-success);
}

.op-bar-fill--error {
  background: var(--cn-danger);
}

.op-bar-fill--indeterminate {
  width: 30%;
  animation: indeterminate 1.5s infinite ease-in-out;
  background: var(--cn-accent);
}

@keyframes indeterminate {
  0% { transform: translateX(-100%); }
  100% { transform: translateX(400%); }
}

/* Detail line */
.op-detail {
  margin-top: 4px;
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 11px;
  color: var(--cn-text-muted, #888);
}

.op-current-file {
  flex: 1;
  min-width: 0;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.op-bytes {
  flex-shrink: 0;
}

.op-elapsed {
  flex-shrink: 0;
}

.op-error-msg {
  margin-top: 4px;
  font-size: 11px;
  color: #ef4444;
  word-break: break-word;
}

/* Transitions */
.op-panel-enter-active,
.op-panel-leave-active {
  transition: opacity 0.25s ease, transform 0.25s ease;
}

.op-panel-enter-from,
.op-panel-leave-to {
  opacity: 0;
  transform: translateY(12px);
}

.op-body-enter-active,
.op-body-leave-active {
  transition: max-height 0.2s ease, opacity 0.2s ease;
  overflow: hidden;
}

.op-body-enter-from,
.op-body-leave-to {
  max-height: 0;
  opacity: 0;
}

.op-item-enter-active,
.op-item-leave-active {
  transition: opacity 0.2s ease, transform 0.2s ease;
}

.op-item-enter-from {
  opacity: 0;
  transform: translateX(12px);
}

.op-item-leave-to {
  opacity: 0;
  transform: translateX(-12px);
}

.op-item-move {
  transition: transform 0.2s ease;
}
</style>
