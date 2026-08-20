<script setup lang="ts">
import { computed } from 'vue'

const props = withDefaults(
  defineProps<{
    os?: string
    distro?: string
    size?: number | string
    badgeSize?: number | string
    showBadge?: boolean
  }>(),
  {
    os: '',
    distro: '',
    size: 22,
    badgeSize: 38,
    showBadge: true,
  }
)

export type OsBrand =
  | 'ubuntu'
  | 'debian'
  | 'centos'
  | 'redhat'
  | 'rocky'
  | 'almalinux'
  | 'fedora'
  | 'alpine'
  | 'arch'
  | 'suse'
  | 'kali'
  | 'amazon'
  | 'openeuler'
  | 'anolis'
  | 'opencloud'
  | 'kylin'
  | 'deepin'
  | 'mint'
  | 'freebsd'
  | 'windows'
  | 'macos'
  | 'linux'
  | 'other'

const brand = computed<OsBrand>(() => {
  const s = `${props.os || ''} ${props.distro || ''}`.toLowerCase()
  if (s.includes('ubuntu')) return 'ubuntu'
  if (s.includes('debian') || s.includes('raspbian')) return 'debian'
  if (s.includes('rocky')) return 'rocky'
  if (s.includes('alma')) return 'almalinux'
  if (s.includes('centos')) return 'centos'
  if (s.includes('rhel') || s.includes('red hat') || s.includes('redhat')) return 'redhat'
  if (s.includes('fedora')) return 'fedora'
  if (s.includes('alpine')) return 'alpine'
  if (s.includes('arch') || s.includes('manjaro') || s.includes('endeavour')) return 'arch'
  if (s.includes('suse') || s.includes('sles')) return 'suse'
  if (s.includes('kali') || s.includes('parrot')) return 'kali'
  if (s.includes('amazon') || s.includes('amzn') || s.includes('aws')) return 'amazon'
  if (s.includes('openeuler') || s.includes('euler')) return 'openeuler'
  if (s.includes('anolis') || s.includes('alinux') || s.includes('alibaba')) return 'anolis'
  if (s.includes('opencloud') || s.includes('tencent') || s.includes('tlinux')) return 'opencloud'
  if (s.includes('kylin') || s.includes('neokylin')) return 'kylin'
  if (s.includes('uos') || s.includes('deepin') || s.includes('uniontech')) return 'deepin'
  if (s.includes('mint')) return 'mint'
  if (s.includes('bsd') || s.includes('freebsd') || s.includes('openbsd')) return 'freebsd'
  if (s.includes('windows') || s.includes('win32') || s.includes('win64')) return 'windows'
  if (s.includes('darwin') || s.includes('mac') || s.includes('osx') || s.includes('apple')) return 'macos'
  if (s.includes('linux')) return 'linux'
  return 'other'
})
</script>

<template>
  <div
    class="os-logo-wrapper"
    :class="[
      `brand-${brand}`,
      { 'is-badge': showBadge }
    ]"
    :style="{
      width: showBadge ? `${badgeSize}px` : 'auto',
      height: showBadge ? `${badgeSize}px` : 'auto',
    }"
  >
    <!-- 1. Ubuntu -->
    <svg v-if="brand === 'ubuntu'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <circle cx="16" cy="16" r="14" fill="none" stroke="currentColor" stroke-width="2.5" stroke-dasharray="18 4 18 4 18 4" />
      <circle cx="6.5" cy="16" r="2.2" fill="currentColor" />
      <circle cx="21" cy="7.8" r="2.2" fill="currentColor" />
      <circle cx="21" cy="24.2" r="2.2" fill="currentColor" />
    </svg>

    <!-- 2. Debian -->
    <svg v-else-if="brand === 'debian'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M16 3.5C9.1 3.5 3.5 9.1 3.5 16S9.1 28.5 16 28.5c7.2 0 12.2-5.4 12-11.4-.2-4.8-4-8.8-8.8-8.8-4.2 0-7.4 3.1-7.4 6.9 0 3.3 2.5 5.7 5.7 5.7 2.4 0 4.1-1.5 4.1-3.6 0-1.7-1.1-2.9-2.7-2.9-1.2 0-2 .8-2 1.9 0 .8.5 1.4 1.3 1.4.4 0 .7-.2.9-.5.1.4.5.6.9.6 1 0 1.8-1 1.8-2.5 0-2.3-1.8-4-4.2-4-2.7 0-4.8 2-4.8 4.8 0 3.1 2.4 5.4 5.7 5.4 3.7 0 6.6-2.9 6.8-6.9.2-4.9-3.7-9.5-9.3-9.5-5.9 0-10.4 4.5-10.4 10.5 0 6.3 4.8 11.2 11.2 11.2 5.8 0 10.3-4.1 10.8-9.8.1-1-.7-1.8-1.7-1.8s-1.8.8-1.9 1.8c-.4 4.5-3.9 7.7-8.2 7.7-5.1 0-9-3.9-9-9 0-4.8 3.6-8.5 8.4-8.5 4.4 0 7.4 3.5 7.3 7.3-.1 3.2-2.3 5.4-5.1 5.4-2.1 0-3.6-1.5-3.6-3.6 0-1.8 1.4-3.2 3.3-3.2 1.4 0 2.3.9 2.3 2.1 0 .6-.4 1.1-.9 1.1-.3 0-.5-.2-.5-.5 0-.6-.5-1-1.1-1-.8 0-1.3.6-1.3 1.4 0 1.1.9 1.9 2.1 1.9 1.6 0 2.9-1.3 2.9-3.1 0-2.3-1.8-4-4.1-4-2.7 0-4.8 2-4.8 4.8 0 3.1 2.3 5.4 5.6 5.4z"
      />
    </svg>

    <!-- 3. CentOS -->
    <svg v-else-if="brand === 'centos'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <g fill="none" stroke="currentColor" stroke-width="2.2" stroke-linejoin="round">
        <path d="M16 4 L28 16 L16 28 L4 16 Z" />
        <path d="M16 8 L24 16 L16 24 L8 16 Z" />
        <line x1="16" y1="4" x2="16" y2="28" />
        <line x1="4" y1="16" x2="28" y2="16" />
      </g>
    </svg>

    <!-- 4. Red Hat / RHEL -->
    <svg v-else-if="brand === 'redhat'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M27.8 18.2c-.3 0-.6.1-.9.2-.8-2.6-3.2-4.5-6.1-4.5-.4 0-.8 0-1.2.1-1.1-3.6-4.5-6.2-8.5-6.2-4.2 0-7.8 2.8-8.8 6.7-.4-.1-.8-.1-1.2-.1-3.5 0-6.4 2.8-6.4 6.3 0 .3 0 .7.1 1C1.9 22.8 5.7 26 10.3 26c4.6 0 8.4-3.2 9.5-7.6 1.4.6 3 1 4.7 1 3.5 0 6.4-2.8 6.4-6.3 0-.3-.1-.6-.2-.9z"
      />
      <path fill="#ffffff" d="M11 13c1.5 0 2.8 1 3.2 2.4-1.2.5-2.6.8-4 .8-2.2 0-4.1-.7-5.5-1.9 1-1.7 3.5-3.3 6.3-3.3z" opacity="0.4" />
    </svg>

    <!-- 5. Rocky Linux -->
    <svg v-else-if="brand === 'rocky'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M16 3C8.8 3 3 8.8 3 16s5.8 13 13 13 13-5.8 13-13S23.2 3 16 3zm0 3.2c5.4 0 9.8 4.4 9.8 9.8 0 2.2-.7 4.3-2 5.9l-6.8-9.8h-2v10.7h-3V12.9h2.1l6.7 9.7c-1.4.9-3 1.4-4.8 1.4-5.4 0-9.8-4.4-9.8-9.8S10.6 6.2 16 6.2z"
      />
    </svg>

    <!-- 6. AlmaLinux -->
    <svg v-else-if="brand === 'almalinux'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <g fill="currentColor">
        <path d="M16 4c-3.3 0-6 2.7-6 6 0 3.3 6 10 6 10s6-6.7 6-10c0-3.3-2.7-6-6-6zm0 8.5c-1.4 0-2.5-1.1-2.5-2.5S14.6 7.5 16 7.5s2.5 1.1 2.5 2.5-1.1 2.5-2.5 2.5z" />
        <circle cx="8" cy="20" r="3.5" opacity="0.85" />
        <circle cx="24" cy="20" r="3.5" opacity="0.85" />
        <circle cx="16" cy="26" r="3.2" opacity="0.7" />
      </g>
    </svg>

    <!-- 7. Fedora -->
    <svg v-else-if="brand === 'fedora'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M16 2C8.3 2 2 8.3 2 16s6.3 14 14 14 14-6.3 14-14S23.7 2 16 2zm4.8 8.6h-2.9c-.8 0-1.4.6-1.4 1.4v2.1h4.3v2.8h-4.3v6.5h-3.2v-6.5h-2.1v-2.8h2.1V12c0-2.3 1.9-4.2 4.2-4.2h3.3v2.8z"
      />
    </svg>

    <!-- 8. Alpine Linux -->
    <svg v-else-if="brand === 'alpine'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M16 4.5 L3.5 25.5 L12.5 25.5 L16 19.5 L19.5 25.5 L28.5 25.5 Z M16 11.5 L21.5 21 L10.5 21 Z"
      />
    </svg>

    <!-- 9. Arch Linux -->
    <svg v-else-if="brand === 'arch'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M16 4.2C14.7 7.4 7.6 22.8 4 27.2c3.5-1.8 6.4-3.4 9-4.8.4-.2.8-.4 1.2-.6-1-.9-1.8-2-2.3-3.2 1.8 1.4 3.7 2.4 5.8 3 .5.1 1 .2 1.6.2.7-.2 1.3-.4 1.9-.7 1.4-.7 2.6-1.7 3.5-2.9-.6 1.4-1.6 2.6-2.8 3.5 1.5.7 3.1 1.6 4.8 2.6 1.8 1 3.5 2 5.3 2.9-3.6-4.4-10.7-19.8-12-23z"
      />
    </svg>

    <!-- 10. openSUSE / SUSE -->
    <svg v-else-if="brand === 'suse'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M16 3C8.8 3 3 8.8 3 16s5.8 13 13 13 13-5.8 13-13S23.2 3 16 3zm5.8 9.5c.8 0 1.5.7 1.5 1.5s-.7 1.5-1.5 1.5-1.5-.7-1.5-1.5.7-1.5 1.5-1.5zM16 23.5c-4.4 0-8-2.7-8-6s3.6-6 8-6 8 2.7 8 6-3.6 6-8 6z"
      />
      <circle cx="16" cy="17.5" r="2.5" fill="#ffffff" opacity="0.6" />
    </svg>

    <!-- 11. Kali Linux -->
    <svg v-else-if="brand === 'kali'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M16 2C8.3 2 2 8.3 2 16s6.3 14 14 14 14-6.3 14-14S23.7 2 16 2zm7.2 8.5c-.8 2.3-2.5 4.1-4.8 5-1.3.5-2.7.7-4.1.7-1.2 0-2.4-.2-3.5-.6l3.8-3.8c1.2-.3 2.4-.4 3.6-.2 1.7.3 3.3 1.3 4.2 2.7.3-.6.5-1.3.6-2 .1-.6.1-1.2 0-1.8h.2zM8.8 21.5c.8-2.3 2.5-4.1 4.8-5 1.3-.5 2.7-.7 4.1-.7 1.2 0 2.4.2 3.5.6l-3.8 3.8c-1.2.3-2.4.4-3.6.2-1.7-.3-3.3-1.3-4.2-2.7-.3.6-.5 1.3-.6 2-.1.6-.1 1.2 0 1.8h-.2z"
      />
    </svg>

    <!-- 12. Amazon Linux / AWS -->
    <svg v-else-if="brand === 'amazon'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M6 19.5c5.5 3.5 14.5 3.5 20 0 .5-.3 1.2.2.8.7-2.6 3-8.8 4.8-10.8 4.8-2.1 0-7.7-1.7-10.8-4.8-.4-.5.3-1 .8-.7z"
      />
      <path
        fill="currentColor"
        d="M16 6c-3.8 0-6.5 2.8-6.5 6.8 0 4.1 2.8 6.2 6.5 6.2 3.6 0 6.5-2.2 6.5-6.2C22.5 8.8 19.8 6 16 6zm0 10.2c-2.2 0-3.5-1.7-3.5-3.8 0-2.1 1.3-3.8 3.5-3.8s3.5 1.7 3.5 3.8c0 2.1-1.3 3.8-3.5 3.8z"
      />
    </svg>

    <!-- 13. openEuler / EulerOS -->
    <svg v-else-if="brand === 'openeuler'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <circle cx="16" cy="16" r="12" fill="none" stroke="currentColor" stroke-width="2.5" />
      <circle cx="16" cy="16" r="6" fill="none" stroke="currentColor" stroke-width="2" />
      <circle cx="16" cy="7" r="2.5" fill="currentColor" />
      <circle cx="23" cy="20" r="2.5" fill="currentColor" />
      <circle cx="9" cy="20" r="2.5" fill="currentColor" />
    </svg>

    <!-- 14. Alibaba Cloud Linux / Anolis OS (龙蜥) -->
    <svg v-else-if="brand === 'anolis'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M16 3C8.8 3 3 8.8 3 16s5.8 13 13 13 13-5.8 13-13S23.2 3 16 3zm0 4.5c4.8 0 8.7 3.9 8.7 8.7 0 2.4-1 4.6-2.6 6.2l-3.6-3.6c.6-.7 1-1.6 1-2.6 0-2.1-1.7-3.8-3.8-3.8s-3.8 1.7-3.8 3.8c0 1 .4 1.9 1 2.6l-3.6 3.6c-1.6-1.6-2.6-3.8-2.6-6.2 0-4.8 3.9-8.7 8.7-8.7z"
      />
    </svg>

    <!-- 15. OpenCloudOS / TencentOS -->
    <svg v-else-if="brand === 'opencloud'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M21.5 11c-.5-3.4-3.4-6-6.9-6-2.8 0-5.2 1.7-6.3 4.1C5.7 9.8 4 12 4 14.7 4 18.2 6.8 21 10.3 21h11.2c2.5 0 4.5-2 4.5-4.5 0-2.3-1.7-4.2-3.9-4.5h-.6z"
      />
    </svg>

    <!-- 16. Kylin / 银河麒麟 -->
    <svg v-else-if="brand === 'kylin'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <g fill="currentColor">
        <polygon points="16,3 20,12 29,13 22,20 24,29 16,24 8,29 10,20 3,13 12,12" />
      </g>
    </svg>

    <!-- 17. Deepin / UOS (统信) -->
    <svg v-else-if="brand === 'deepin'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="none"
        stroke="currentColor"
        stroke-width="3"
        stroke-linecap="round"
        d="M10 16c0-3.3 2.7-6 6-6s6 2.7 6 6-2.7 6-6 6-6-2.7-6-6zm12 0c0-3.3 2.7-6 6-6"
      />
      <circle cx="16" cy="16" r="3" fill="currentColor" />
    </svg>

    <!-- 18. Linux Mint -->
    <svg v-else-if="brand === 'mint'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <rect x="4" y="4" width="24" height="24" rx="6" fill="none" stroke="currentColor" stroke-width="2.5" />
      <path fill="currentColor" d="M10 11v10h4v-6c0-1.1.9-2 2-2s2 .9 2 2v6h4v-6c0-3.3-2.7-6-6-6s-6 2.7-6 6z" />
    </svg>

    <!-- 19. FreeBSD / BSD -->
    <svg v-else-if="brand === 'freebsd'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <circle cx="16" cy="17" r="11" fill="currentColor" />
      <path fill="currentColor" d="M9 10C8 6 10 3 12 2c0 2 0 5-2 8zm14 0c1-4-1-7-3-8 0 2 0 5 2 8z" />
      <circle cx="12" cy="15" r="2" fill="#ffffff" />
      <circle cx="20" cy="15" r="2" fill="#ffffff" />
    </svg>

    <!-- 20. Windows -->
    <svg v-else-if="brand === 'windows'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path fill="currentColor" d="M4 6.8 L13.6 5.5 L13.6 14.8 L4 14.8 Z M15.2 5.2 L28 3.5 L28 14.8 L15.2 14.8 Z M4 16.4 L13.6 16.4 L13.6 25.7 L4 24.4 Z M15.2 16.4 L28 16.4 L28 27.7 L15.2 26 Z" />
    </svg>

    <!-- 21. macOS / Apple -->
    <svg v-else-if="brand === 'macos'" viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M22.5 16.8c0-3.2 2.6-4.8 2.7-4.9-1.5-2.2-3.8-2.5-4.6-2.5-2-.2-3.8 1.2-4.8 1.2-1 0-2.5-1.1-4.1-1.1-2.1 0-4.1 1.2-5.1 3.1-2.2 3.8-.6 9.4 1.5 12.5 1 1.5 2.3 3.1 3.9 3 1.6-.1 2.2-1 4.1-1s2.5 1 4.1 1c1.7 0 2.8-1.5 3.8-3 1.2-1.7 1.7-3.4 1.7-3.5-.1 0-3.2-1.2-3.2-4.8zm-3.5-9.6c.9-1.1 1.5-2.6 1.3-4.2-1.3.1-2.9.9-3.8 1.9-.8.9-1.5 2.5-1.3 4 1.5.1 3-.7 3.8-1.7z"
      />
    </svg>

    <!-- 22. Generic Linux (Tux Silhouette) -->
    <svg v-else viewBox="0 0 32 32" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M16 2.5c-3.6 0-6.2 2.8-6.2 6.5 0 1.2.3 2.3.8 3.2C8.7 13.8 7 16.4 7 19.8c0 3.2 1.4 5.9 3.5 7.4-1.2 1-2.5 1.8-3.5 2.1-.5.2-.8.6-.8 1.1 0 .6.5 1.1 1.1 1.1h17.4c.6 0 1.1-.5 1.1-1.1 0-.5-.3-.9-.8-1.1-1-.3-2.3-1.1-3.5-2.1 2.1-1.5 3.5-4.2 3.5-7.4 0-3.4-1.7-6-3.6-7.6.5-.9.8-2 .8-3.2 0-3.7-2.6-6.5-6.2-6.5zm-2.2 6.2c.7 0 1.2.6 1.2 1.2 0 .7-.6 1.2-1.2 1.2-.7 0-1.2-.5-1.2-1.2 0-.6.5-1.2 1.2-1.2zm4.4 0c.7 0 1.2.6 1.2 1.2 0 .7-.6 1.2-1.2 1.2-.7 0-1.2-.5-1.2-1.2 0-.6.5-1.2 1.2-1.2z"
      />
    </svg>
  </div>
</template>

<style scoped lang="scss">
.os-logo-wrapper {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;

  &.is-badge {
    border-radius: 50%;
    color: #ffffff;
    box-shadow: 0 2px 6px rgba(0, 0, 0, 0.12);
  }

  &:not(.is-badge) {
    color: currentColor;
  }

  // 1. Ubuntu: Ubuntu warm orange
  &.brand-ubuntu.is-badge {
    background: linear-gradient(135deg, #e95420 0%, #ba3908 100%);
  }
  // 2. Debian: Crimson Red
  &.brand-debian.is-badge {
    background: linear-gradient(135deg, #d70a53 0%, #9e0036 100%);
  }
  // 3. CentOS: Deep Navy Blue
  &.brand-centos.is-badge {
    background: linear-gradient(135deg, #262577 0%, #15144f 100%);
  }
  // 4. Red Hat: Bright Red
  &.brand-redhat.is-badge {
    background: linear-gradient(135deg, #ee0000 0%, #a80000 100%);
  }
  // 5. Rocky Linux: Emerald Green
  &.brand-rocky.is-badge {
    background: linear-gradient(135deg, #10b981 0%, #047857 100%);
  }
  // 6. AlmaLinux: Indigo Blue
  &.brand-almalinux.is-badge {
    background: linear-gradient(135deg, #2b5dd1 0%, #173b96 100%);
  }
  // 7. Fedora: Classic Fedora Blue
  &.brand-fedora.is-badge {
    background: linear-gradient(135deg, #294172 0%, #152445 100%);
  }
  // 8. Alpine: Deep Slate Cyan
  &.brand-alpine.is-badge {
    background: linear-gradient(135deg, #0d597f 0%, #06344d 100%);
  }
  // 9. Arch: Sky Cyan
  &.brand-arch.is-badge {
    background: linear-gradient(135deg, #1793d1 0%, #0b5e87 100%);
  }
  // 10. SUSE: Gecko Green
  &.brand-suse.is-badge {
    background: linear-gradient(135deg, #73ba25 0%, #4d8014 100%);
  }
  // 11. Kali: Blue Dragon
  &.brand-kali.is-badge {
    background: linear-gradient(135deg, #2777ff 0%, #1048b0 100%);
  }
  // 12. Amazon Linux: AWS Orange
  &.brand-amazon.is-badge {
    background: linear-gradient(135deg, #ff9900 0%, #cc7a00 100%);
  }
  // 13. openEuler: Euler Navy Blue
  &.brand-openeuler.is-badge {
    background: linear-gradient(135deg, #002fa7 0%, #001859 100%);
  }
  // 14. Anolis / Alinux: Alibaba Dragon Orange
  &.brand-anolis.is-badge {
    background: linear-gradient(135deg, #ff6a00 0%, #c44d00 100%);
  }
  // 15. OpenCloudOS / Tencent: Tencent Tech Blue
  &.brand-opencloud.is-badge {
    background: linear-gradient(135deg, #0052d9 0%, #003691 100%);
  }
  // 16. Kylin: Crimson Star
  &.brand-kylin.is-badge {
    background: linear-gradient(135deg, #c8102e 0%, #80071a 100%);
  }
  // 17. Deepin / UOS: Deepin Azure
  &.brand-deepin.is-badge {
    background: linear-gradient(135deg, #0078d7 0%, #004b87 100%);
  }
  // 18. Mint: Mint Lime
  &.brand-mint.is-badge {
    background: linear-gradient(135deg, #87cf3e 0%, #5d9722 100%);
  }
  // 19. FreeBSD: Beastie Red
  &.brand-freebsd.is-badge {
    background: linear-gradient(135deg, #ab2b28 0%, #6e1513 100%);
  }
  // 20. Windows: Microsoft Blue
  &.brand-windows.is-badge {
    background: linear-gradient(135deg, #0078d6 0%, #005a9e 100%);
  }
  // 21. macOS: Sleek Apple Charcoal
  &.brand-macos.is-badge {
    background: linear-gradient(135deg, #333333 0%, #1a1a1a 100%);
  }
  // 22. Generic Linux / Other: Slate Gray
  &.brand-linux.is-badge,
  &.brand-other.is-badge {
    background: linear-gradient(135deg, #475569 0%, #334155 100%);
  }

  .os-svg {
    display: block;
    flex-shrink: 0;
  }
}
</style>
