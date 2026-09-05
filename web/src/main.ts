import { createApp } from 'vue'
import { createPinia } from 'pinia'
import router from './router'
import App from './App.vue'
import './styles/global.scss'
// asciinema-player 动态创建终端 DOM，不会携带 Vue scoped 的 data-v 属性，
// 因此官方样式必须全局引入，否则 canvas/SVG 会失去定位和尺寸规则。
import 'asciinema-player/dist/bundle/asciinema-player.css'

const app = createApp(App)
app.use(createPinia())
app.use(router)
app.mount('#app')