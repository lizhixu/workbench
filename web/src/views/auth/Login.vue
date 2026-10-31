<script setup lang="ts">
import { ref, onMounted, onUnmounted } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { NCard, NForm, NFormItem, NInput, NButton, NSpace, NIcon, useMessage } from 'naive-ui'
import { PersonOutline, LockClosedOutline } from '@vicons/ionicons5'
import { useAuthStore } from '../../stores/auth'

const router = useRouter()
const route = useRoute()
const auth = useAuthStore()
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
    <!-- 1. 精密工程蓝图网格（底层常驻暗色网格） -->
    <div class="bg-grid-base" aria-hidden="true" />

    <!-- 2. 鼠标探照灯揭示的高亮网格（仅在光标周围细腻被照亮，纯粹线条无光圈光斑） -->
    <div class="bg-grid-spotlight" aria-hidden="true" />

    <!-- 3. 工程坐标十字准星（模拟控制面板精密定位锚点） -->
    <div class="tech-crosshairs" aria-hidden="true">
      <span class="ch-mark ch-tl">+</span>
      <span class="ch-mark ch-tr">+</span>
      <span class="ch-mark ch-bl">+</span>
      <span class="ch-mark ch-br">+</span>
    </div>

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
  background-color: #090d16;
  padding: 16px;
  box-sizing: border-box;
  overflow: hidden;

  // 鼠标跟随默认居中，支持运行时动态更新
  --mouse-x: 50%;
  --mouse-y: 50%;
  --spotlight-opacity: 0.5;
}

// 1. 基础静态网格：超细线条常驻底板（纯净深色无色晕）
.bg-grid-base {
  position: absolute;
  inset: 0;
  pointer-events: none;
  background-image:
    linear-gradient(to right, rgba(255, 255, 255, 0.025) 1px, transparent 1px),
    linear-gradient(to bottom, rgba(255, 255, 255, 0.025) 1px, transparent 1px);
  background-size: 36px 36px;
  mask-image: radial-gradient(ellipse 75% 65% at 50% 48%, #000 20%, transparent 85%);
  -webkit-mask-image: radial-gradient(ellipse 75% 65% at 50% 48%, #000 20%, transparent 85%);
}

// 4. 探照灯高亮网格：鼠标扫过区域网格优雅浮现（Linear/Vercel 式工程师质感）
.bg-grid-spotlight {
  position: absolute;
  inset: 0;
  pointer-events: none;
  background-image:
    linear-gradient(to right, rgba(129, 140, 248, 0.16) 1px, transparent 1px),
    linear-gradient(to bottom, rgba(129, 140, 248, 0.16) 1px, transparent 1px);
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

// 5. 四个工程十字定位标：对齐锚点
.tech-crosshairs {
  position: absolute;
  inset: 0;
  pointer-events: none;
  max-width: 880px;
  max-height: 640px;
  margin: auto;

  .ch-mark {
    position: absolute;
    font-family: 'JetBrains Mono', Consolas, monospace;
    font-size: 15px;
    line-height: 1;
    color: rgba(255, 255, 255, 0.16);
    user-select: none;
  }

  .ch-tl { top: 32px; left: 32px; }
  .ch-tr { top: 32px; right: 32px; }
  .ch-bl { bottom: 32px; left: 32px; }
  .ch-br { bottom: 32px; right: 32px; }
}

// 6. 登录卡片容器与卡片质感
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
      rgba(99, 102, 241, 0.7) 50%,
      transparent 100%
    );
    z-index: 3;
    pointer-events: none;
  }
}

.login-card {
  width: 100%;
  border-radius: 12px;
  background: rgba(19, 27, 46, 0.82) !important;
  backdrop-filter: blur(16px);
  -webkit-backdrop-filter: blur(16px);
  border: 1px solid rgba(255, 255, 255, 0.08) !important;
  box-shadow:
    0 24px 48px -12px rgba(0, 0, 0, 0.7),
    0 0 0 1px rgba(255, 255, 255, 0.03);

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
        color: #f8fafc;
        line-height: 1.3;
        letter-spacing: 0.2px;
      }

      .login-sub {
        font-size: 11.5px;
        color: #94a3b8;
        letter-spacing: 0.1px;
      }
    }
  }

  .input-icon {
    color: #64748b;
    margin-right: 2px;
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
  color: #64748b;
  letter-spacing: 0.2px;
}

// 移动端响应式处理：屏幕较窄时隐藏边缘装饰，防止喧宾夺主
@media (max-width: 768px) {
  .tech-crosshairs {
    display: none;
  }

  .login-bg {
    padding: 12px;
  }

  .login-card-wrap {
    width: 100%;
  }
}
</style>
