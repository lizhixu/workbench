<script setup lang="ts">
import { ref, onMounted, onUnmounted } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import {
  NCard,
  NForm,
  NFormItem,
  NInput,
  NButton,
  NSpace,
  NIcon,
  NTooltip,
  useMessage,
} from 'naive-ui'
import {
  PersonOutline,
  LockClosedOutline,
  SunnyOutline,
  MoonOutline,
} from '@vicons/ionicons5'
import { useAuthStore } from '../../stores/auth'
import { useSettingsStore } from '../../stores/settings'

const router = useRouter()
const route = useRoute()
const auth = useAuthStore()
const settings = useSettingsStore()
const message = useMessage()

const username = ref('admin')
const password = ref('')
const loading = ref(false)

const containerRef = ref<HTMLElement | null>(null)
let targetX = 0
let targetY = 0
let currentX = 0
let currentY = 0
let rafId: number | null = null
let isHovered = false

function updatePosition() {
  // 阻尼平滑插值（Lerp factor 0.1），带来探照灯掠过蓝图网格的自然物理质感与平滑感
  const ease = 0.1
  currentX += (targetX - currentX) * ease
  currentY += (targetY - currentY) * ease

  if (containerRef.value) {
    containerRef.value.style.setProperty('--mouse-x', `${currentX.toFixed(1)}px`)
    containerRef.value.style.setProperty('--mouse-y', `${currentY.toFixed(1)}px`)
    containerRef.value.style.setProperty('--spotlight-opacity', isHovered ? '1' : '0.5')
  }

  rafId = requestAnimationFrame(updatePosition)
}

function handlePointerMove(e: PointerEvent) {
  if (!containerRef.value) return
  const rect = containerRef.value.getBoundingClientRect()
  targetX = e.clientX - rect.left
  targetY = e.clientY - rect.top
  isHovered = true
}

function handlePointerLeave() {
  isHovered = false
  // 移出视口时，聚光灯平滑向中心回落
  if (containerRef.value) {
    targetX = containerRef.value.clientWidth / 2
    targetY = containerRef.value.clientHeight / 2
  }
}

onMounted(() => {
  if (containerRef.value) {
    targetX = containerRef.value.clientWidth / 2
    targetY = containerRef.value.clientHeight / 2
    currentX = targetX
    currentY = targetY
    containerRef.value.style.setProperty('--mouse-x', `${currentX}px`)
    containerRef.value.style.setProperty('--mouse-y', `${currentY}px`)
    containerRef.value.style.setProperty('--spotlight-opacity', '0.5')
  }

  // 仅在支持精密指针的设备（PC鼠标等）上启动交互跟随，移动端保持静止居中
  if (window.matchMedia('(pointer: fine)').matches) {
    window.addEventListener('pointermove', handlePointerMove, { passive: true })
    document.addEventListener('pointerleave', handlePointerLeave)
    rafId = requestAnimationFrame(updatePosition)
  }
})

onUnmounted(() => {
  if (rafId !== null) {
    cancelAnimationFrame(rafId)
  }
  window.removeEventListener('pointermove', handlePointerMove)
  document.removeEventListener('pointerleave', handlePointerLeave)
})

async function handleLogin() {
  if (!username.value || !password.value) {
    message.warning('请输入用户名和密码')
    return
  }
  loading.value = true
  try {
    await auth.login(username.value, password.value)
    message.success('登录成功')
    const redirect = (route.query.redirect as string) || '/hosts'
    router.push(redirect)
  } catch (e: any) {
    message.error(e.message || '登录失败')
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div ref="containerRef" class="login-bg">
    <!-- 0. 顶部右上角亮/暗主题快速切换 -->
    <div class="theme-toggle-wrap">
      <NTooltip trigger="hover">
        <template #trigger>
          <NButton
            quaternary
            circle
            size="small"
            class="theme-toggle-btn"
            @click="settings.toggleTheme"
          >
            <template #icon>
              <NIcon size="18">
                <SunnyOutline v-if="settings.themeMode === 'dark'" />
                <MoonOutline v-else />
              </NIcon>
            </template>
          </NButton>
        </template>
        <span>切换至 {{ settings.themeMode === 'dark' ? '亮色' : '暗色' }}模式</span>
      </NTooltip>
    </div>

    <!-- 1. 精密工程蓝图网格（底层常驻工程网格） -->
    <div class="bg-grid-base" aria-hidden="true" />

    <!-- 2. 鼠标探照灯揭示的高亮网格（仅在光标周围细腻被照亮，纯粹线条无光圈光斑） -->
    <div class="bg-grid-spotlight" aria-hidden="true" />

    <!-- 4. 核心登录卡片 -->
    <div class="login-card-wrap">
      <!-- 顶部精密微高光流线 -->
      <div class="card-glow-beam" aria-hidden="true" />

      <NCard class="login-card" size="large" :bordered="false">
        <template #header>
          <div class="login-header">
            <img src="/k-works.png" class="login-logo-img" alt="Logo" />
            <div class="login-title-wrap">
              <h1 class="login-title">Watchman 云堡垒机</h1>
              <span class="login-sub">主机管理与安全运维审计平台</span>
            </div>
          </div>
        </template>

        <NForm @keyup.enter="handleLogin">
          <NFormItem label="用户名">
            <NInput v-model:value="username" placeholder="请输入用户名" size="medium">
              <template #prefix>
                <NIcon :component="PersonOutline" class="input-icon" />
              </template>
            </NInput>
          </NFormItem>
          <NFormItem label="密码">
            <NInput
              v-model:value="password"
              type="password"
              show-password-on="click"
              placeholder="请输入密码"
              size="medium"
            >
              <template #prefix>
                <NIcon :component="LockClosedOutline" class="input-icon" />
              </template>
            </NInput>
          </NFormItem>
          <NSpace vertical :size="12">
            <NButton
              type="primary"
              block
              size="medium"
              :loading="loading"
              class="login-btn"
              @click="handleLogin"
            >
              登录控制台
            </NButton>
          </NSpace>
        </NForm>
        <div class="hint">默认凭据 admin / admin · 登录后请及时修改</div>
      </NCard>
    </div>
  </div>
</template>

<style scoped lang="scss">
.login-bg {
  position: relative;
  width: 100vw;
  height: 100vh;
  height: 100dvh;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 16px;
  box-sizing: border-box;
  overflow: hidden;

  // 鼠标跟随默认居中，支持运行时动态更新
  --mouse-x: 50%;
  --mouse-y: 50%;
  --spotlight-opacity: 0.5;

  // 暗色模式设计令牌（默认基准）
  --login-bg-color: #090d16;
  --login-grid-base-color: rgba(255, 255, 255, 0.025);
  --login-grid-spotlight-color: rgba(129, 140, 248, 0.16);
  --login-card-bg: rgba(19, 27, 46, 0.82);
  --login-card-border: 1px solid rgba(255, 255, 255, 0.08);
  --login-card-shadow: 0 24px 48px -12px rgba(0, 0, 0, 0.7), 0 0 0 1px rgba(255, 255, 255, 0.03);
  --login-beam-color: rgba(99, 102, 241, 0.7);
  --login-title-color: #f8fafc;
  --login-sub-color: #94a3b8;
  --login-icon-color: #64748b;
  --login-hint-color: #64748b;
  --login-theme-btn-color: #94a3b8;
  --login-theme-btn-hover: #f8fafc;
  // 输入框 autofill 适配：Chrome 自动填充强制给 input 涂白底，
  // 在暗色卡片上极为刺眼。用 -webkit-box-shadow 内阴影覆盖 autofill 背景色。
  --login-input-autofill-bg: #1a233a;
  --login-input-autofill-text: #d1d5db;

  background: var(--login-bg-color);
  transition: background 0.3s ease;
}

// 亮色覆盖：`:global()` 必须包裹整条选择器。只写 `:global(html.light) .login-bg`
// 会被 scoped 编译器截断成裸 `html.light`（.login-bg 被丢弃），变量落在 html 上被
// 元素自身的暗色声明压制，亮色覆盖从未生效——故整条选择器都要进 :global()。
:global(html.light .login-bg) {
  // 亮色模式设计令牌（白纸工程制图：明亮、清爽、层次分明——
  // 毫米图纸线肉眼可辨、卡片是高对比纯白浮层，而非暗色设计的淡色涂装）
  --login-bg-color: linear-gradient(180deg, #f9fafd 0%, #eef2f8 100%);
  --login-grid-base-color: rgba(100, 116, 139, 0.09);
  --login-grid-spotlight-color: rgba(99, 102, 241, 0.3);
  --login-card-bg: rgba(255, 255, 255, 0.92);
  --login-card-border: 1px solid #e2e8f0;
  --login-card-shadow:
    0 1px 2px rgba(15, 23, 42, 0.04),
    0 8px 24px -6px rgba(15, 23, 42, 0.08),
    0 24px 48px -16px rgba(15, 23, 42, 0.12);
  --login-beam-color: rgba(99, 102, 241, 0.6);
  --login-title-color: #0f172a;
  --login-sub-color: #64748b;
  --login-icon-color: #94a3b8;
  --login-hint-color: #64748b;
  --login-theme-btn-color: #64748b;
  --login-theme-btn-hover: #0f172a;
  --login-input-autofill-bg: #ffffff;
  --login-input-autofill-text: #334155;
}

// 0. 主题切换按钮：右上角固定
.theme-toggle-wrap {
  position: absolute;
  top: 20px;
  right: 24px;
  z-index: 10;

  .theme-toggle-btn {
    color: var(--login-theme-btn-color);
    transition: color 0.2s ease;

    &:hover {
      color: var(--login-theme-btn-hover);
    }
  }
}

// 1. 基础静态网格：超细线条常驻底板（纯净深色无色晕）
.bg-grid-base {
  position: absolute;
  inset: 0;
  pointer-events: none;
  background-image:
    linear-gradient(to right, var(--login-grid-base-color) 1px, transparent 1px),
    linear-gradient(to bottom, var(--login-grid-base-color) 1px, transparent 1px);
  background-size: 36px 36px;
  mask-image: radial-gradient(ellipse 75% 65% at 50% 48%, #000 20%, transparent 85%);
  -webkit-mask-image: radial-gradient(ellipse 75% 65% at 50% 48%, #000 20%, transparent 85%);
}

// 2. 探照灯高亮网格：鼠标扫过区域网格优雅浮现（Linear/Vercel 式工程师质感）
.bg-grid-spotlight {
  position: absolute;
  inset: 0;
  pointer-events: none;
  background-image:
    linear-gradient(to right, var(--login-grid-spotlight-color) 1px, transparent 1px),
    linear-gradient(to bottom, var(--login-grid-spotlight-color) 1px, transparent 1px);
  background-size: 36px 36px;
  mask-image: radial-gradient(
    circle 340px at var(--mouse-x, 50%) var(--mouse-y, 50%),
    black 0%,
    rgba(0, 0, 0, 0.5) 50%,
    transparent 85%
  );
  -webkit-mask-image: radial-gradient(
    circle 340px at var(--mouse-x, 50%) var(--mouse-y, 50%),
    black 0%,
    rgba(0, 0, 0, 0.5) 50%,
    transparent 85%
  );
  opacity: var(--spotlight-opacity, 0.5);
  transition: opacity 0.4s ease;
}

// 4. 登录卡片容器与卡片质感
.login-card-wrap {
  position: relative;
  width: 410px;
  max-width: 100%;
  z-index: 2;

  // 卡片顶缘的精密微光导线
  .card-glow-beam {
    position: absolute;
    top: 0;
    left: 12%;
    right: 12%;
    height: 1px;
    background: linear-gradient(
      90deg,
      transparent 0%,
      var(--login-beam-color) 50%,
      transparent 100%
    );
    z-index: 3;
    pointer-events: none;
    transition: background 0.3s ease;
  }
}

.login-card {
  width: 100%;
  border-radius: 12px;
  background: var(--login-card-bg) !important;
  backdrop-filter: blur(16px);
  -webkit-backdrop-filter: blur(16px);
  border: var(--login-card-border) !important;
  box-shadow: var(--login-card-shadow);
  transition: background 0.3s ease, border-color 0.3s ease, box-shadow 0.3s ease;

  .login-header {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 4px 0 2px;

    .login-logo-img {
      width: 42px;
      height: 42px;
      object-fit: contain;
      border: none;
      box-shadow: none;
      flex-shrink: 0;
    }

    .login-title-wrap {
      display: flex;
      flex-direction: column;
      gap: 3px;

      .login-title {
        margin: 0;
        font-size: 17px;
        font-weight: 700;
        color: var(--login-title-color);
        line-height: 1.3;
        letter-spacing: 0.2px;
        transition: color 0.3s ease;
      }

      .login-sub {
        font-size: 11.5px;
        color: var(--login-sub-color);
        letter-spacing: 0.1px;
        transition: color 0.3s ease;
      }
    }
  }

  .input-icon {
    color: var(--login-icon-color);
    margin-right: 2px;
    transition: color 0.3s ease;
  }

  // Chrome/Edge 自动填充：强制给 input 涂白底 + 黄底高亮，
  // 在暗色磨砂卡片上极为刺眼。用 -webkit-box-shadow 内阴影覆盖
  // autofill 背景色（Chrome 不会覆盖 box-shadow），同时用
  // -webkit-text-fill-color 固定文字色。transition 5000s 是为了
  // 让 autofill 注入时的颜色变化不触发动画闪烁。
  :deep(input:-webkit-autofill),
  :deep(input:-webkit-autofill:focus),
  :deep(input:-webkit-autofill:hover),
  :deep(textarea:-webkit-autofill),
  :deep(select:-webkit-autofill) {
    -webkit-text-fill-color: var(--login-input-autofill-text);
    -webkit-box-shadow: 0 0 0 1000px var(--login-input-autofill-bg) inset;
    caret-color: var(--login-input-autofill-text);
    transition: background-color 5000s ease-in-out 0s,
      color 5000s ease-in-out 0s,
      -webkit-text-fill-color 5000s ease-in-out 0s;
  }

  .login-btn {
    font-weight: 600;
    letter-spacing: 0.5px;
    box-shadow: 0 4px 12px rgba(99, 102, 241, 0.25);
  }
}

.hint {
  margin-top: 14px;
  text-align: center;
  font-size: 11.5px;
  color: var(--login-hint-color);
  letter-spacing: 0.2px;
  transition: color 0.3s ease;
}

// 移动端响应式处理：屏幕较窄时隐藏边缘装饰，防止喧宾夺主
@media (max-width: 768px) {
  .theme-toggle-wrap {
    top: 12px;
    right: 12px;
  }

  .login-bg {
    padding: 12px;
  }

  .login-card-wrap {
    width: 100%;
  }
}
</style>
