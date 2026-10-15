<!-- 统一图标出口：所有 icon 走这里，避免各组件散写 svg 与重复属性。 -->
<template>
  <svg
    class="icon"
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.75"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
    focusable="false"
  >
    <path v-for="(d, i) in paths" :key="i" :d="d" />
  </svg>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { icons, type IconName } from '@/config/icons'

const props = withDefaults(defineProps<{ name: IconName; size?: number }>(), {
  size: 18,
})

const paths = computed(() => icons[props.name] ?? [])
</script>

<style scoped>
.icon {
  flex: none;
  display: block;
}
</style>
