<script setup lang="ts">
import { ref } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { NCard, NForm, NFormItem, NInput, NButton, NSpace, useMessage } from 'naive-ui'
import { useAuthStore } from '../../stores/auth'

const router = useRouter()
const route = useRoute()
const auth = useAuthStore()
const message = useMessage()

const username = ref('admin')
const password = ref('')
const loading = ref(false)

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
  <div class="login-bg">
    <NCard class="login-card" size="large" :bordered="true">
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
          <NInput v-model:value="username" placeholder="用户名" />
        </NFormItem>
        <NFormItem label="密码">
          <NInput
            v-model:value="password"
            type="password"
            show-password-on="click"
            placeholder="密码"
          />
        </NFormItem>
        <NSpace vertical>
          <NButton
            type="primary"
            block
            :loading="loading"
            @click="handleLogin"
          >
            登录
          </NButton>
        </NSpace>
      </NForm>
      <div class="hint">默认账号 admin / admin，登录后请及时修改密码</div>
    </NCard>
  </div>
</template>

<style scoped lang="scss">
.login-bg {
  height: 100vh;
  height: 100dvh;
  display: flex;
  align-items: center;
  justify-content: center;
  background: linear-gradient(135deg, #1f2937 0%, #111827 100%);
  padding: 16px;
  box-sizing: border-box;
}
.login-card {
  width: 400px;
  max-width: 100%;
  border-radius: 12px;
  box-shadow: 0 10px 30px -10px rgba(0, 0, 0, 0.5);

  .login-header {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 6px 0;

    .login-logo-img {
      width: 44px;
      height: 44px;
      object-fit: contain;
      border: none;
      box-shadow: none;
      flex-shrink: 0;
    }

    .login-title-wrap {
      display: flex;
      flex-direction: column;
      gap: 2px;

      .login-title {
        margin: 0;
        font-size: 17px;
        font-weight: 700;
        color: var(--text-primary);
        line-height: 1.3;
      }

      .login-sub {
        font-size: 11.5px;
        color: var(--text-secondary);
      }
    }
  }
}
.hint {
  margin-top: 12px;
  text-align: center;
  font-size: 12px;
  color: #9ca3af;
}

@media (max-width: 480px) {
  .login-card {
    width: 100%;
  }
}
</style>