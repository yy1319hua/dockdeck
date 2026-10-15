import { createApp } from 'vue'
import { createPinia } from 'pinia'
import './styles/tokens.css'
import './styles/base.css'
import App from './App.vue'
import router from './router'
import { installGuard } from './router/guard'

const app = createApp(App)

// pinia 必须先装：守卫在导航时取 store，晚于 pinia 会拿不到实例。
app.use(createPinia())
installGuard(router)
app.use(router)
app.mount('#app')
