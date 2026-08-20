<script setup lang="ts">
import { ref } from 'vue'
import {
  NButton,
  NModal,
  NTabs,
  NTabPane,
  NInput,
  NSwitch,
  NForm,
  NFormItem,
  NSpace,
  useMessage,
  NIcon,
} from 'naive-ui'
import { AddCircleOutline, CloudOutline } from '@vicons/ionicons5'

const message = useMessage()
const showModal = ref(false)
const activeVendor = ref('aliyun')

const form = ref({
  remark: '',
  accessKeyId: '',
  accessKeySecret: '',
  autoBind: true,
})

function saveCloudAccount() {
  // No backend cloud-account API yet (Phase 2 feature in Agent.md).
  message.info('云账号对接接口尚未开放（Phase 2 功能），当前无法保存凭据')
  showModal.value = false
}
</script>

<template>
  <div class="cloud-assets-view">
    <div class="top-bar">
      <div class="title">云资产与云账号</div>
      <NButton type="primary" size="small" @click="showModal = true">
        <template #icon>
          <NIcon><AddCircleOutline /></NIcon>
        </template>
        添加云账号
      </NButton>
    </div>

    <div class="asset-card-placeholder">
      <NIcon size="48" color="#6366f1"><CloudOutline /></NIcon>
      <div class="text">暂未关联公有云账号，点击「添加云账号」自动导入阿里云/腾讯云/AWS/Azure 实例。</div>
    </div>

    <!-- 添加云账号 Modal (参截图 c00d3ff3239994086878287e86ac900a.png) -->
    <NModal
      v-model:show="showModal"
      preset="card"
      title="添加云账号"
      style="width: 580px"
    >
      <div class="cloud-account-modal">
        <!-- 云厂商页签 -->
        <NTabs v-model:value="activeVendor" type="segment">
          <NTabPane name="aliyun" tab="阿里云" />
          <NTabPane name="tencent" tab="腾讯云" />
          <NTabPane name="aws" tab="AWS" />
          <NTabPane name="azure" tab="Microsoft Azure" />
        </NTabs>

        <NForm label-placement="top" style="margin-top: 16px">
          <NFormItem label="云账号备注">
            <NInput v-model:value="form.remark" placeholder="请输入云账号备注" />
          </NFormItem>

          <NFormItem label="AccessKey ID">
            <NInput v-model:value="form.accessKeyId" placeholder="请输入 AccessKey ID" />
          </NFormItem>

          <NFormItem label="AccessKey Secret">
            <template #label>
              <span>AccessKey Secret </span>
              <a href="#" class="help-link">如何获取AccessKey?</a>
            </template>
            <NInput
              v-model:value="form.accessKeySecret"
              type="password"
              show-password-on="click"
              placeholder="请输入 AccessKey Secret"
            />
          </NFormItem>

          <NFormItem label="自动绑定云账号下的所有主机 (安装牧云主机助手)">
            <NSwitch v-model:value="form.autoBind" />
          </NFormItem>
        </NForm>

        <NSpace justify="end" style="margin-top: 14px">
          <NButton @click="showModal = false">返回</NButton>
          <NButton type="primary" @click="saveCloudAccount">保存</NButton>
        </NSpace>
      </div>
    </NModal>
  </div>
</template>

<style scoped lang="scss">
.cloud-assets-view {
  display: flex;
  flex-direction: column;
  gap: 16px;

  .top-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;

    .title {
      font-size: 16px;
      font-weight: 700;
    }
  }

  .asset-card-placeholder {
    padding: 60px 0;
    text-align: center;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 12px;
    color: #9ca3af;
    font-size: 13px;
  }

  .help-link {
    font-size: 12px;
    color: #818cf8;
    text-decoration: none;
    margin-left: 6px;
  }
}
</style>
