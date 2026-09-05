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
  if (s.includes('suse') || s.includes('sles') || s.includes('opensuse')) return 'suse'
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
    <!-- 1. Ubuntu (Official Circle of Friends: 3 dots + 3 arcs) -->
    <svg v-if="brand === 'ubuntu'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <g fill="currentColor">
        <circle cx="128" cy="128" r="46.5" />
        <circle cx="196.2" cy="79.4" r="18.2" />
        <circle cx="196.2" cy="176.6" r="18.2" />
        <circle cx="59.6" cy="128" r="18.2" />
        <path d="M128 32 C82.8 32 45.1 63.8 34.6 106.3 C42.1 106.6 48.6 111.4 51.5 118.4 C60.2 76.9 96.9 45.4 140.7 45.4 C151.7 45.4 162.2 47.4 171.9 51.1 L184.2 36.9 C167.3 33.7 148.1 32 128 32 Z" />
        <path d="M221.4 106.3 C210.9 63.8 173.2 32 128 32 C124.5 32 121.1 32.2 117.7 32.5 L124.9 46.1 C125.9 46 127 46 128 46 C168.6 46 202.2 75.3 209.6 114 C216.5 113.1 223.3 116.8 226.5 123.3 C227.1 117.8 224.7 112.1 221.4 106.3 Z" />
        <path d="M128 224 C167.3 224 201.2 198.8 214.2 163.2 C208 160.8 203.4 155.6 202.1 149 C191.6 182.2 162.5 205.8 128 205.8 C93.5 205.8 64.4 182.2 53.9 149 C52.6 155.6 48 160.8 41.8 163.2 C54.8 198.8 88.7 224 128 224 Z" />
      </g>
    </svg>

    <!-- 2. Debian (Authentic Debian Swirl) -->
    <svg v-else-if="brand === 'debian'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M128 20C68.4 20 20 68.4 20 128c0 59.6 48.4 108 108 108 59.6 0 108-48.4 108-108 0-11.8-1.9-23.2-5.4-33.8-2.6 6.8-6.1 13.2-10.4 19.1 2.5 4.7 3.8 10 3.8 15.6 0 47.5-38.5 86-86 86s-86-38.5-86-86 38.5-86 86-86c23.2 0 44.3 9.2 59.8 24.2 4.2-4.5 9-8.4 14.3-11.6C188 35.8 159.4 20 128 20zm50.6 61.2c-1.8 1.4-3.4 3-5 4.7-12.2-10.8-28.2-17.4-45.6-17.4-37.6 0-68 30.4-68 68s30.4 68 68 68c34.8 0 63.4-26.1 67.4-59.8 1.9-2.5 4.1-4.8 6.5-6.7-3.1 36.2-33.6 64.5-70.9 64.5-39.8 0-72-32.2-72-72s32.2-72 72-72c19.4 0 37.1 7.7 50.1 20.2 2.6-2.6 5.5-4.8 8.6-6.7-1.3-.3-2.7-.6-4.1-.8zM128 92c-19.9 0-36 16.1-36 36s16.1 36 36 36 36-16.1 36-36c0-6.1-1.5-11.8-4.2-16.8 1.8-1.5 3.8-2.8 6-3.8 3.9 6 6.2 13.1 6.2 20.6 0 22.1-17.9 40-40 40s-40-17.9-40-40 17.9-40 40-40c9.8 0 18.7 3.5 25.6 9.4 1.8-1.6 3.8-3 6-4.1C149.7 96.6 139.3 92 128 92z"
      />
    </svg>

    <!-- 3. CentOS (Official 4-Diamond Multi-facet Cross) -->
    <svg v-else-if="brand === 'centos'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <g fill="currentColor">
        <path d="M128 32 L160 84 L128 116 L96 84 Z" opacity="0.95" />
        <path d="M224 128 L172 160 L140 128 L172 96 Z" opacity="0.85" />
        <path d="M128 224 L96 172 L128 140 L160 172 Z" opacity="0.95" />
        <path d="M32 128 L84 96 L116 128 L84 160 Z" opacity="0.85" />
        <path d="M168 88 L188 108 L148 148 L128 128 Z" opacity="0.6" />
        <path d="M168 168 L148 188 L108 148 L128 128 Z" opacity="0.6" />
        <path d="M88 168 L68 148 L108 108 L128 128 Z" opacity="0.6" />
        <path d="M88 88 L108 68 L148 108 L128 128 Z" opacity="0.6" />
      </g>
    </svg>

    <!-- 4. Red Hat / RHEL (Official Red Fedora Hat Silhouette) -->
    <svg v-else-if="brand === 'redhat'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <g fill="currentColor">
        <path d="M216.5 138.8c-1.3-.2-2.7-.3-4-.3-13.8 0-25.7 8.2-31.1 20-3.6-14.7-16.8-25.7-32.6-25.7-4.4 0-8.6.8-12.5 2.3C132.8 116.7 114.2 104 92.5 104c-22.1 0-41 13.2-49.4 32.2-4.1-1.8-8.6-2.8-13.3-2.8C13.3 133.4 0 146.7 0 163.2c0 2.2.3 4.4.8 6.5C11.5 204.6 47.9 232 92.5 232c45.4 0 82.3-28.5 92.3-64.7 9.8 4.7 20.8 7.4 32.4 7.4 20.9 0 38.3-15.6 40.5-35.9h-.2z" />
        <path fill="#ffffff" opacity="0.3" d="M102.5 134.5c14.2 0 25.8 9.5 29.5 22.8-11.1 4.7-24.1 7.4-37.5 7.4-20.7 0-38.6-6.6-51.8-17.9 9.5-16 33.2-31.1 59.8-32.3z" />
      </g>
    </svg>

    <!-- 5. Rocky Linux (Official Rocky R-Peak Loop) -->
    <svg v-else-if="brand === 'rocky'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <g fill="currentColor">
        <path d="M128 24C70.6 24 24 70.6 24 128s46.6 104 104 104 104-46.6 104-104S185.4 24 128 24zm0 28c42 0 76 34 76 76 0 17.5-5.9 33.6-15.8 46.5L138 102h-18v82h-22V98h24.5l52.5 74.8c-11.4 7.5-25 11.9-39.7 11.9-42 0-76-34-76-76.7 0-42 34-76 76.7-76z" />
      </g>
    </svg>

    <!-- 6. AlmaLinux (Official 4-Way Connected Node Ring) -->
    <svg v-else-if="brand === 'almalinux'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <g fill="currentColor">
        <circle cx="128" cy="52" r="28" />
        <circle cx="128" cy="204" r="28" />
        <circle cx="52" cy="128" r="28" />
        <circle cx="204" cy="128" r="28" />
        <path d="M128 80 C128 106 106 128 80 128 C106 128 128 150 128 176 C128 150 150 128 176 128 C150 128 128 106 128 80 Z" opacity="0.8" />
      </g>
    </svg>

    <!-- 7. Fedora (Official Infinity 'f' Bubble) -->
    <svg v-else-if="brand === 'fedora'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M128 16C66.1 16 16 66.1 16 128s50.1 112 112 112 112-50.1 112-112S189.9 16 128 16zm42.7 74.7h-25.6c-7.1 0-12.8 5.7-12.8 12.8v18.7h38.4v24.9h-38.4v57.6h-28.8v-57.6H84.3v-24.9h19.2v-18.7c0-20.5 16.7-37.2 37.2-37.2h30v24.4z"
      />
    </svg>

    <!-- 8. Alpine Linux (Official 3-Mountain Peaks) -->
    <svg v-else-if="brand === 'alpine'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M128 42 L24 214 L98 214 L128 162 L158 214 L232 214 Z M128 98 L172 176 L84 176 Z"
      />
    </svg>

    <!-- 9. Arch Linux (Official Swoosh Arch Archway) -->
    <svg v-else-if="brand === 'arch'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M128 32C117.6 57.6 60.8 180.8 32 216c28-14.4 51.2-27.2 72-38.4 3.2-1.6 6.4-3.2 9.6-4.8-8-7.2-14.4-16-18.4-25.6 14.4 11.2 29.6 19.2 46.4 24 4 .8 8 1.6 12.8 1.6 5.6-1.6 10.4-3.2 15.2-5.6 11.2-5.6 20.8-13.6 28-23.2-4.8 11.2-12.8 20.8-22.4 28 12 5.6 24.8 12.8 38.4 20.8 14.4 8 28 16 42.4 23.2-28.8-35.2-85.6-158.4-96-184z"
      />
    </svg>

    <!-- 10. openSUSE / SUSE (Official Chameleon Profile Eye) -->
    <svg v-else-if="brand === 'suse'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <g fill="currentColor">
        <path d="M128 24C70.6 24 24 70.6 24 128s46.6 104 104 104 104-46.6 104-104S185.4 24 128 24zm46.4 76c6.4 0 12 5.6 12 12s-5.6 12-12 12-12-5.6-12-12 5.6-12 12-12zM128 188c-35.2 0-64-21.6-64-48s28.8-48 64-48 64 21.6 64 48-28.8 48-64 48z" />
        <circle cx="128" cy="140" r="20" fill="#ffffff" opacity="0.75" />
      </g>
    </svg>

    <!-- 11. Kali Linux (Official Kali Dragon Emblem) -->
    <svg v-else-if="brand === 'kali'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M128 16C66.1 16 16 66.1 16 128s50.1 112 112 112 112-50.1 112-112S189.9 16 128 16zm57.6 68c-6.4 18.4-20 32.8-38.4 40-10.4 4-21.6 5.6-32.8 5.6-9.6 0-19.2-1.6-28-4.8l30.4-30.4c9.6-2.4 19.2-3.2 28.8-1.6 13.6 2.4 26.4 10.4 33.6 21.6 2.4-4.8 4-10.4 4.8-16 .8-4.8.8-9.6 0-14.4h1.6zM70.4 172c6.4-18.4 20-32.8 38.4-40 10.4-4 21.6-5.6 32.8-5.6 9.6 0 19.2 1.6 28 4.8l-30.4 30.4c-9.6 2.4-19.2 3.2-28.8 1.6-13.6-2.4-26.4-10.4-33.6-21.6-2.4 4.8-4 10.4-4.8 16-.8 4.8-.8 9.6 0 14.4h-1.6z"
      />
    </svg>

    <!-- 12. Amazon Linux / AWS (Official AWS Smile Arrow) -->
    <svg v-else-if="brand === 'amazon'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <g fill="currentColor">
        <path d="M48 156c44 28 116 28 160 0 4-2.4 9.6 1.6 6.4 5.6-20.8 24-70.4 38.4-86.4 38.4-16.8 0-61.6-13.6-86.4-38.4-3.2-4 2.4-8 6.4-5.6z" />
        <path d="M214 148 L196 142 L202 166 Z" />
        <path d="M128 48c-30.4 0-52 22.4-52 54.4 0 32.8 22.4 49.6 52 49.6s52-17.6 52-49.6c0-32-21.6-54.4-52-54.4zm0 81.6c-17.6 0-28-13.6-28-30.4s10.4-30.4 28-30.4 28 13.6 28 30.4-10.4 30.4-28 30.4z" />
      </g>
    </svg>

    <!-- 13. openEuler (Official Dual Orbit Ring with Core) -->
    <svg v-else-if="brand === 'openeuler'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <g fill="none" stroke="currentColor" stroke-width="20" stroke-linecap="round">
        <circle cx="128" cy="128" r="96" />
        <circle cx="128" cy="128" r="48" />
        <circle cx="128" cy="56" r="14" fill="currentColor" stroke="none" />
        <circle cx="184" cy="160" r="14" fill="currentColor" stroke="none" />
        <circle cx="72" cy="160" r="14" fill="currentColor" stroke="none" />
      </g>
    </svg>

    <!-- 14. Alibaba Cloud Linux / Anolis OS (Dragon/Phoenix Ring) -->
    <svg v-else-if="brand === 'anolis'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M128 24C70.6 24 24 70.6 24 128s46.6 104 104 104 104-46.6 104-104S185.4 24 128 24zm0 36c39 0 70.5 31.5 70.5 70.5 0 19.5-8.1 37.4-21.1 50.4l-29.2-29.2c4.9-5.7 8.1-13 8.1-21.1 0-17-13.8-30.8-30.8-30.8s-30.8 13.8-30.8 30.8c0 8.1 3.2 15.4 8.1 21.1L74.1 180.9C61.1 167.9 53 150 53 130.5 53 91.5 84.5 60 128 60z"
      />
    </svg>

    <!-- 15. OpenCloudOS / TencentOS (Official Cloud Arc Ribbon) -->
    <svg v-else-if="brand === 'opencloud'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M172 88c-4-27.2-27.2-48-55.2-48-22.4 0-41.6 13.6-50.4 32.8C45.6 78.4 32 96 32 117.6c0 28 22.4 50.4 50.4 50.4h89.6c20 0 36-16 36-36 0-18.4-13.6-33.6-31.2-36h-4.8z"
      />
    </svg>

    <!-- 16. Kylin / 银河麒麟 (Official Kylin Crest Star) -->
    <svg v-else-if="brand === 'kylin'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <g fill="currentColor">
        <polygon points="128,24 160,96 232,104 176,160 192,232 128,192 64,232 80,160 24,104 96,96" />
      </g>
    </svg>

    <!-- 17. Deepin / UOS (Official Deepin Infinity Loop) -->
    <svg v-else-if="brand === 'deepin'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <g fill="none" stroke="currentColor" stroke-width="24" stroke-linecap="round">
        <path d="M80 128 C80 101.5 101.5 80 128 80 C154.5 80 176 101.5 176 128 C176 154.5 154.5 176 128 176 C101.5 176 80 154.5 80 128 Z" />
        <path d="M176 128 C176 101.5 197.5 80 224 80" />
      </g>
      <circle cx="128" cy="128" r="24" fill="currentColor" />
    </svg>

    <!-- 18. Linux Mint (Official Mint Leaf LM Badge) -->
    <svg v-else-if="brand === 'mint'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <rect x="32" y="32" width="192" height="192" rx="48" fill="none" stroke="currentColor" stroke-width="20" />
      <path fill="currentColor" d="M80 88v80h32v-48c0-8.8 7.2-16 16-16s16 7.2 16 16v48h32v-48c0-26.5-21.5-48-48-48s-48 21.5-48 48z" />
    </svg>

    <!-- 19. FreeBSD / BSD (Official Beastie Ball & Horns) -->
    <svg v-else-if="brand === 'freebsd'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <g fill="currentColor">
        <circle cx="128" cy="136" r="88" />
        <path d="M72 80 C64 48 80 24 96 16 C80 32 80 56 64 80 Z" />
        <path d="M184 80 C192 48 176 24 160 16 C176 32 176 56 192 80 Z" />
        <circle cx="96" cy="120" r="16" fill="#ffffff" />
        <circle cx="160" cy="120" r="16" fill="#ffffff" />
      </g>
    </svg>

    <!-- 20. Windows (Official Microsoft 4-Square Grid) -->
    <svg v-else-if="brand === 'windows'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <g fill="currentColor">
        <rect x="32" y="32" width="90" height="90" rx="3" />
        <rect x="134" y="32" width="90" height="90" rx="3" />
        <rect x="32" y="134" width="90" height="90" rx="3" />
        <rect x="134" y="134" width="90" height="90" rx="3" />
      </g>
    </svg>

    <!-- 21. macOS / Apple (Official Bitten Apple) -->
    <svg v-else-if="brand === 'macos'" viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <path
        fill="currentColor"
        d="M180 134.4c0-25.6 20.8-38.4 21.6-39.2-12-17.6-30.4-20-36.8-20-16-1.6-30.4 9.6-38.4 9.6-8 0-20-8.8-32.8-8.8-16.8 0-32.8 9.6-40.8 24.8-17.6 30.4-4.8 75.2 12 100 8 12 18.4 24.8 31.2 24 12.8-.8 17.6-8 32.8-8s20 8 32.8 8c13.6 0 22.4-12 30.4-24 9.6-13.6 13.6-27.2 13.6-28-.8 0-25.6-9.6-25.6-38.4zM152 57.6c7.2-8.8 12-20.8 10.4-33.6-10.4.8-23.2 7.2-30.4 15.2-6.4 7.2-12 20-10.4 32 12 .8 24-5.6 30.4-13.6z"
      />
    </svg>

    <!-- 22. Generic Linux (Official Tux Silhouette) -->
    <svg v-else viewBox="0 0 256 256" class="os-svg" :style="{ width: `${size}px`, height: `${size}px` }">
      <g fill="currentColor">
        <path d="M128 20c-28.8 0-49.6 22.4-49.6 52 0 9.6 2.4 18.4 6.4 25.6-20 12.8-33.6 33.6-33.6 60.8 0 25.6 11.2 47.2 28 59.2-9.6 8-20 14.4-28 16.8-4 1.6-6.4 4.8-6.4 8.8 0 4.8 4 8.8 8.8 8.8h148.8c4.8 0 8.8-4 8.8-8.8 0-4-2.4-7.2-6.4-8.8-8-2.4-18.4-8.8-28-16.8 16.8-12 28-33.6 28-59.2 0-27.2-13.6-48-33.6-60.8 4-7.2 6.4-16 6.4-25.6 0-29.6-20.8-52-49.6-52z" />
        <circle cx="110" cy="70" r="5" fill="#ffffff" />
        <circle cx="146" cy="70" r="5" fill="#ffffff" />
      </g>
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
    box-shadow: 0 2px 8px rgba(0, 0, 0, 0.16);
  }

  &:not(.is-badge) {
    color: currentColor;
  }

  // 1. Ubuntu: Official Warm Auburn Orange
  &.brand-ubuntu.is-badge {
    background: linear-gradient(135deg, #e95420 0%, #ba3908 100%);
  }
  // 2. Debian: Official Crimson Red
  &.brand-debian.is-badge {
    background: linear-gradient(135deg, #d70a53 0%, #9e0036 100%);
  }
  // 3. CentOS: Deep Cent Purple/Navy
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
