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
    <NCard class="login-card" title="Watchman 云堡垒机" size="large" :bordered="true">
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
  display: flex;
  align-items: center;
  justify-content: center;
  background: linear-gradient(135deg, #1f2937 0%, #111827 100%);
}
.login-card {
  width: 380px;
}
.hint {
  margin-top: 12px;
  text-align: center;
  font-size: 12px;
  color: #9ca3af;
}
</style>