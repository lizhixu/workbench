<script setup lang="ts">
import { ref } from 'vue'
import {
  NCard, NForm, NFormItem, NInput, NButton, NSpace, useMessage,
} from 'naive-ui'
import { updatePassword } from '../../api/users'

const message = useMessage()
const oldPwd = ref('')
const newPwd = ref('')
const confirmPwd = ref('')
const loading = ref(false)

async function submit() {
  if (!oldPwd.value || !newPwd.value || !confirmPwd.value) {
    message.warning('请填写所有字段')
    return
  }
  if (newPwd.value !== confirmPwd.value) {
    message.warning('两次输入的新密码不一致')
    return
  }
  if (newPwd.value.length < 6) {
    message.warning('新密码至少 6 位')
    return
  }
  loading.value = true
  try {
    await updatePassword(oldPwd.value, newPwd.value)
    message.success('密码修改成功')
    oldPwd.value = ''
    newPwd.value = ''
    confirmPwd.value = ''
  } catch (e: any) {
    message.error(e.message)
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <NCard title="修改密码" style="max-width: 480px">
    <NForm label-placement="top">
      <NFormItem label="当前密码">
        <NInput v-model:value="oldPwd" type="password" show-password-on="click" placeholder="当前密码" />
      </NFormItem>
      <NFormItem label="新密码">
        <NInput v-model:value="newPwd" type="password" show-password-on="click" placeholder="至少 6 位" />
      </NFormItem>
      <NFormItem label="确认新密码">
        <NInput
          v-model:value="confirmPwd"
          type="password"
          show-password-on="click"
          placeholder="再次输入新密码"
          @keyup.enter="submit"
        />
      </NFormItem>
    </NForm>
    <NSpace justify="end">
      <NButton type="primary" :loading="loading" @click="submit">确认修改</NButton>
    </NSpace>
  </NCard>
</template>