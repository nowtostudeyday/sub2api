<template>
  <BaseDialog
    :show="show"
    :title="t('admin.usage.transcript.title')"
    width="extra-wide"
    @close="emit('close')"
  >
    <div class="h-[min(62vh,36rem)] overflow-y-auto">
      <div v-if="loading" class="py-12 text-center text-sm text-gray-500 dark:text-gray-400">
        {{ t('common.loading') }}
      </div>

      <div v-else-if="errorMessage" class="py-12 text-center text-sm text-gray-500 dark:text-gray-400">
        {{ errorMessage }}
      </div>

      <div v-else-if="transcript" class="space-y-4">
        <div class="flex flex-wrap gap-x-6 gap-y-2 text-sm text-gray-600 dark:text-gray-300">
          <span>{{ t('admin.usage.transcript.model') }}: {{ transcript.model || '—' }}</span>
          <span>{{ t('admin.usage.transcript.turnKind') }}: {{ turnKindLabel }}</span>
          <span>{{ t('admin.usage.transcript.createdAt') }}: {{ formattedCreatedAt }}</span>
        </div>

        <section v-if="transcript.user_input">
          <h4 class="mb-1 text-sm font-medium text-gray-900 dark:text-gray-100">
            {{ t('admin.usage.transcript.userInput') }}
            <span v-if="transcript.user_input_truncated" class="ml-1 text-xs text-amber-600 dark:text-amber-400">
              {{ t('admin.usage.transcript.truncated') }}
            </span>
          </h4>
          <pre class="whitespace-pre-wrap break-words rounded bg-gray-50 p-3 text-sm text-gray-800 dark:bg-gray-800 dark:text-gray-200">{{ transcript.user_input }}</pre>
        </section>

        <section v-if="transcript.output_text">
          <h4 class="mb-1 text-sm font-medium text-gray-900 dark:text-gray-100">
            {{ t('admin.usage.transcript.output') }}
            <span v-if="transcript.output_truncated" class="ml-1 text-xs text-amber-600 dark:text-amber-400">
              {{ t('admin.usage.transcript.truncated') }}
            </span>
          </h4>
          <pre class="whitespace-pre-wrap break-words rounded bg-gray-50 p-3 text-sm text-gray-800 dark:bg-gray-800 dark:text-gray-200">{{ transcript.output_text }}</pre>
        </section>

        <section v-if="transcript.output_reasoning">
          <h4 class="mb-1 text-sm font-medium text-gray-900 dark:text-gray-100">
            {{ t('admin.usage.transcript.reasoning') }}
          </h4>
          <pre class="whitespace-pre-wrap break-words rounded bg-gray-50 p-3 text-sm text-gray-600 dark:bg-gray-800 dark:text-gray-400">{{ transcript.output_reasoning }}</pre>
        </section>

        <section v-if="transcript.tool_output">
          <h4 class="mb-1 text-sm font-medium text-gray-900 dark:text-gray-100">
            {{ t('admin.usage.transcript.toolOutput') }}
            <span v-if="transcript.tool_output_truncated" class="ml-1 text-xs text-amber-600 dark:text-amber-400">
              {{ t('admin.usage.transcript.truncated') }}
            </span>
          </h4>
          <pre class="whitespace-pre-wrap break-words rounded bg-gray-50 p-3 text-sm text-gray-600 dark:bg-gray-800 dark:text-gray-400">{{ transcript.tool_output }}</pre>
        </section>

        <section v-if="transcript.instructions_full">
          <h4 class="mb-1 text-sm font-medium text-gray-900 dark:text-gray-100">
            {{ t('admin.usage.transcript.instructions') }}
          </h4>
          <pre class="whitespace-pre-wrap break-words rounded bg-gray-50 p-3 text-sm text-gray-600 dark:bg-gray-800 dark:text-gray-400">{{ transcript.instructions_full }}</pre>
        </section>
        <p v-else-if="transcript.instructions_hash" class="text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.usage.transcript.instructionsOmitted') }}
        </p>
      </div>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import BaseDialog from '@/components/common/BaseDialog.vue'
import { adminUsageAPI, type CodexTranscript } from '@/api/admin/usage'
import { formatDateTime } from '@/utils/format'

const props = defineProps<{ show: boolean; requestId: string }>()
const emit = defineEmits<{ (e: 'close'): void }>()

const { t } = useI18n()

const transcript = ref<CodexTranscript | null>(null)
const loading = ref(false)
const errorMessage = ref('')

const formattedCreatedAt = computed(() =>
  transcript.value ? formatDateTime(transcript.value.created_at) : '—'
)

const turnKindLabel = computed(() => {
  const kind = transcript.value?.turn_kind
  if (!kind) return '—'
  return t(`admin.usage.transcript.kinds.${kind}`)
})

// 每次打开都重新拉取：留痕只保留 5 天，缓存旧数据没有意义
const load = async () => {
  transcript.value = null
  errorMessage.value = ''
  if (!props.requestId) {
    errorMessage.value = t('admin.usage.transcript.notFound')
    return
  }
  loading.value = true
  const data = await adminUsageAPI.getCodexTranscript(props.requestId)
  loading.value = false
  if (!data) {
    errorMessage.value = t('admin.usage.transcript.notFound')
    return
  }
  transcript.value = data
}

watch(
  () => [props.show, props.requestId],
  ([show]) => {
    if (show) load()
  },
  { immediate: true }
)
</script>
