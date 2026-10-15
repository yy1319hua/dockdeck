<!--
  S6.1 应用根：只挂 RouterView。
  S6.2 在此包一层鉴权/错误外壳，S6.3 换成侧栏 + 顶栏布局。
-->
<template>
  <RouterView />
</template>

<script setup lang="ts">
// 路由已注册；此处暂不需要任何局部状态。
</script>
