import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

export default defineConfig({
  prefetch: { prefetchAll: true },
  vite: {
    preview: {
      allowedHosts: ['mink-trusted-vigorously.ngrok-free.app'],
    },
  },
  integrations: [
    starlight({
      title: 'image-patch-operator',
      components: {
        Head: './src/components/Head.astro',
      },
      social: [
        { icon: 'github', label: 'GitHub', href: 'https://github.com/pinclr/image-patcher' },
        { icon: 'external', label: 'Artifact Hub', href: 'https://artifacthub.io/packages/helm/image-patcher/image-patcher' },
      ],
      defaultLocale: 'root',
      locales: {
        root: {
          label: 'English',
          lang: 'en',
        },
        'zh-cn': {
          label: '中文',
          lang: 'zh-CN',
        },
      },
      sidebar: [
        {
          label: 'Guides',
          translations: { 'zh-CN': '指南' },
          items: [
            {
              label: 'Overview',
              link: '/',
              translations: { 'zh-CN': '概览' },
            },
            {
              label: 'Getting Started',
              link: '/getting-started/',
              translations: { 'zh-CN': '快速开始' },
            },
          ],
        },
        {
          label: 'Reference',
          translations: { 'zh-CN': '参考' },
          items: [
            {
              label: 'CRD Reference',
              link: '/crd-reference/',
              translations: { 'zh-CN': 'CRD 参考' },
            },
            {
              label: 'Advanced Features',
              link: '/advanced/',
              translations: { 'zh-CN': '高级功能' },
            },
          ],
        },
        {
          label: 'Contributing',
          translations: { 'zh-CN': '贡献' },
          items: [
            {
              label: 'Development Guide',
              link: '/development/',
              translations: { 'zh-CN': '开发指南' },
            },
          ],
        },
      ],
    }),
  ],
});
